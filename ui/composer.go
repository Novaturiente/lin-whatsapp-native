package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"

	"lin-whatsapp-native/store"
)

type Composer struct {
	c        *Conversation
	root     *gtk.Box
	banner   *gtk.Box // "Replying to…" / "Editing"
	bannerTx *gtk.Label
	view     *gtk.TextView
	scroll   *gtk.ScrolledWindow
	fitted   bool // the height cap uses the measured line height
	send     *gtk.Button
	mic      *gtk.Button
	recBox   *gtk.Box
	recLabel *gtk.Label

	reply     *store.Message
	editing   *store.Message
	typingAt  int // generation counter for the "stopped typing" timer
	composing bool
	// Unsent text per chat, kept while the app runs.
	// shortcut: lost on quit; save to the store if that bites.
	drafts    map[string]draft
	restoring bool // a draft is being put back: not typing

	members     []string          // group members, for @mentions
	mentions    map[string]string // "@Name" inserted, to its JID
	mentionPop  *gtk.Popover
	mentionList *gtk.ListBox
	hits        []person // rows of mentionList
	query       string   // text typed after the @

	rec      *exec.Cmd
	recPath  string
	recStart time.Time
}

func newComposer(c *Conversation) *Composer {
	k := &Composer{c: c, root: gtk.NewBox(gtk.OrientationVertical, 4), view: gtk.NewTextView(),
		mentions: map[string]string{}, drafts: map[string]draft{}}
	k.root.SetMarginStart(8)
	k.root.SetMarginEnd(8)
	k.root.SetMarginTop(6)
	k.root.SetMarginBottom(8)

	k.banner = gtk.NewBox(gtk.OrientationHorizontal, 6)
	k.banner.AddCSSClass("quote")
	k.bannerTx = gtk.NewLabel("")
	k.bannerTx.SetXAlign(0)
	k.bannerTx.SetHExpand(true)
	k.bannerTx.SetEllipsize(pango.EllipsizeEnd)
	closeBtn := gtk.NewButtonFromIconName("window-close-symbolic")
	closeBtn.AddCSSClass("flat")
	closeBtn.ConnectClicked(func() { k.cancel() })
	k.banner.Append(k.bannerTx)
	k.banner.Append(closeBtn)
	k.banner.SetVisible(false)
	k.root.Append(k.banner)

	row := gtk.NewBox(gtk.OrientationHorizontal, 4)
	row.Append(k.attachButton())
	row.Append(k.stickerButton())
	row.Append(k.emojiButton())

	k.view.SetWrapMode(gtk.WrapWordChar)
	k.view.SetAcceptsTab(false)
	k.view.SetTopMargin(6)
	k.view.SetBottomMargin(6)
	k.view.SetLeftMargin(10)
	k.view.SetRightMargin(10)
	k.view.AddCSSClass("card")
	k.scroll = gtk.NewScrolledWindow()
	// External: no scrollbar, whose minimum height made the empty box two
	// lines tall. The box follows the text up to maxLines; see fit.
	k.scroll.SetPolicy(gtk.PolicyNever, gtk.PolicyExternal)
	k.scroll.SetPropagateNaturalHeight(true)
	k.scroll.SetMaxContentHeight(maxLines*20 + 12) // until fit measures the real line height
	k.scroll.SetHExpand(true)
	k.scroll.SetVAlign(gtk.AlignEnd)
	k.scroll.SetChild(k.view)
	scroll := k.scroll
	row.Append(scroll)

	k.send = gtk.NewButtonFromIconName("paper-plane-symbolic")
	k.send.AddCSSClass("suggested-action")
	k.send.AddCSSClass("circular")
	k.send.SetTooltipText("Send (Enter)")
	k.send.ConnectClicked(k.submit)
	k.mic = gtk.NewButtonFromIconName("audio-input-microphone-symbolic")
	k.mic.AddCSSClass("circular")
	k.mic.SetTooltipText("Record a voice message")
	k.mic.ConnectClicked(k.startRecording)
	for _, b := range []*gtk.Button{k.send, k.mic} {
		b.SetVAlign(gtk.AlignEnd)
		row.Append(b)
	}
	k.root.Append(row)
	k.root.Append(k.recordingBar())

	buf := k.view.Buffer()
	buf.ConnectChanged(k.changed)

	keys := gtk.NewEventControllerKey()
	keys.SetPropagationPhase(gtk.PhaseCapture) // before the TextView inserts a newline or pastes
	keys.ConnectKeyPressed(k.key)
	k.view.AddController(keys)

	k.mentionList = gtk.NewListBox()
	k.mentionPop = gtk.NewPopover()
	k.mentionPop.SetChild(k.mentionList)
	k.mentionPop.SetAutohide(false)
	k.mentionPop.SetPosition(gtk.PosTop)
	k.mentionPop.SetParent(k.view)
	k.mentionList.ConnectRowActivated(func(row *gtk.ListBoxRow) { k.pickMention(row.Index()) })
	k.changed()
	return k
}

type person struct{ name, jid string }

type draft struct {
	text     string
	mentions map[string]string
}

// keepDraft saves the box's text for the chat being left. Called before
// the conversation switches chats; an edit in progress is not a draft.
func (k *Composer) keepDraft() {
	chat := k.c.chat
	if chat == "" {
		return
	}
	if text := k.text(); k.editing == nil && strings.TrimSpace(text) != "" {
		k.drafts[chat] = draft{text, k.mentions}
	} else {
		delete(k.drafts, chat)
	}
}

func (k *Composer) text() string {
	start, end := k.view.Buffer().Bounds()
	return k.view.Buffer().Text(start, end, false)
}

func (k *Composer) focus() { k.view.GrabFocus() }

// reset clears everything when another chat opens.
func (k *Composer) reset() {
	k.stopTyping()
	k.cancelRecording()
	k.reply, k.editing = nil, nil
	k.banner.SetVisible(false)
	d := k.drafts[k.c.chat]
	k.restoring = true
	k.view.Buffer().SetText(d.text)
	k.restoring = false
	k.mentions = d.mentions
	if k.mentions == nil {
		k.mentions = map[string]string{}
	}
	k.members = nil
	if chat := k.c.chat; isGroup(chat) {
		go func() {
			list := k.c.w.c.Members(chat)
			glib.IdleAdd(func() {
				if k.c.chat == chat {
					k.members = list
				}
			})
		}()
	}
}

// cancel leaves reply or edit mode, or closes the mention list. It reports
// whether there was anything to cancel, so Esc otherwise reaches the page.
func (k *Composer) cancel() bool {
	if k.mentionPop.Visible() {
		k.mentionPop.Popdown()
		return true
	}
	if k.rec != nil {
		k.cancelRecording()
		return true
	}
	if k.reply == nil && k.editing == nil {
		return false
	}
	if k.editing != nil {
		k.view.Buffer().SetText("")
	}
	k.reply, k.editing = nil, nil
	k.banner.SetVisible(false)
	return true
}

func (k *Composer) replyTo(m *store.Message) {
	k.editing, k.reply = nil, m
	k.bannerTx.SetText("Replying to " + k.c.w.c.Name(m.Sender) + ": " + firstLine(previewOf(m)))
	k.banner.SetVisible(true)
	k.focus()
}

func (k *Composer) edit(m *store.Message) {
	k.reply, k.editing = nil, m
	k.bannerTx.SetText("Editing message")
	k.banner.SetVisible(true)
	k.view.Buffer().SetText(m.Text)
	k.focus()
}

func previewOf(m *store.Message) string { return store.Preview(m) }

func (k *Composer) changed() {
	empty := strings.TrimSpace(k.text()) == ""
	k.send.SetVisible(!empty || k.editing != nil)
	k.mic.SetVisible(empty && k.editing == nil && k.rec == nil)
	if !empty && !k.restoring {
		k.startTyping()
	}
	k.updateMentions()
	glib.IdleAdd(k.fit) // after the view has laid out the new text
}

// maxLines is how tall the box grows before it scrolls instead.
const maxLines = 10

// fit caps the box at maxLines of the current font; past that it scrolls.
func (k *Composer) fit() {
	if k.fitted {
		return
	}
	if lineH := k.view.IterLocation(k.view.Buffer().StartIter()).Height(); lineH > 0 {
		k.scroll.SetMaxContentHeight(maxLines*lineH + k.view.TopMargin() + k.view.BottomMargin())
		k.fitted = true
	}
}

func (k *Composer) startTyping() {
	if !k.composing {
		k.composing = true
		k.c.w.c.Typing(k.c.chat, true)
	}
	k.typingAt++
	gen := k.typingAt
	glib.TimeoutSecondsAdd(4, func() bool {
		if gen == k.typingAt {
			k.stopTyping()
		}
		return false
	})
}

func (k *Composer) stopTyping() {
	if k.composing {
		k.composing = false
		k.c.w.c.Typing(k.c.chat, false)
	}
}

func (k *Composer) key(keyval, _ uint, state gdk.ModifierType) bool {
	ctrl := state&gdk.ControlMask != 0
	if k.mentionPop.Visible() {
		row := k.mentionList.SelectedRow()
		i := 0
		if row != nil {
			i = row.Index()
		}
		switch keyval {
		case gdk.KEY_Up, gdk.KEY_Down:
			if keyval == gdk.KEY_Up {
				i--
			} else {
				i++
			}
			if i >= 0 && i < len(k.hits) {
				k.mentionList.SelectRow(k.mentionList.RowAtIndex(i))
			}
			return true
		case gdk.KEY_Return, gdk.KEY_KP_Enter, gdk.KEY_Tab:
			k.pickMention(i)
			return true
		}
	}
	switch {
	case keyval == gdk.KEY_Return || keyval == gdk.KEY_KP_Enter:
		if state&gdk.ShiftMask != 0 {
			return false // newline
		}
		k.submit()
		return true
	case ctrl && keyval == gdk.KEY_v:
		return k.paste()
	case keyval == gdk.KEY_Up && strings.TrimSpace(k.text()) == "":
		return k.c.focusMessages() // like a terminal: up leaves the empty prompt
	case keyval == gdk.KEY_Escape:
		if !k.cancel() {
			k.c.focusMessages()
		}
		return true
	}
	return false
}

func (k *Composer) submit() {
	text := strings.TrimSpace(k.text())
	chat := k.c.chat
	switch {
	case chat == "":
		return
	case k.editing != nil:
		if text != "" && text != k.editing.Text {
			k.c.w.c.Edit(k.editing, text)
		}
	case text == "":
		return
	default:
		text, mentions := k.resolveMentions(text)
		k.c.w.c.SendText(chat, text, k.reply, mentions)
	}
	k.reply, k.editing = nil, nil
	k.banner.SetVisible(false)
	k.view.Buffer().SetText("")
	k.mentions = map[string]string{}
	delete(k.drafts, chat)
	k.stopTyping()
}

// --- mentions ---------------------------------------------------------------

var mentionQuery = regexp.MustCompile(`(?:^|\s)@([^\s@]*)$`)

// updateMentions offers group members while the word before the cursor
// starts with @.
func (k *Composer) updateMentions() {
	if len(k.members) == 0 {
		k.mentionPop.Popdown()
		return
	}
	buf := k.view.Buffer()
	cursor := buf.IterAtMark(buf.GetInsert())
	lineStart := cursor.Copy()
	lineStart.SetLineOffset(0)
	match := mentionQuery.FindStringSubmatch(buf.Text(lineStart, cursor, false))
	if match == nil {
		k.mentionPop.Popdown()
		return
	}
	query := match[1]
	k.mentionList.RemoveAll()
	var hits []person
	for _, j := range k.members {
		if n := k.c.w.c.Name(j); contains(n, query) || strings.Contains(j, query) {
			hits = append(hits, person{n, j})
		}
	}
	sort.Slice(hits, func(a, b int) bool { return hits[a].name < hits[b].name })
	if len(hits) == 0 {
		k.mentionPop.Popdown()
		return
	}
	for i, p := range hits {
		if i == 8 {
			break
		}
		row := gtk.NewListBoxRow()
		l := gtk.NewLabel(p.name)
		l.SetXAlign(0)
		l.SetMarginStart(6)
		l.SetMarginEnd(6)
		l.SetMarginTop(4)
		l.SetMarginBottom(4)
		row.SetChild(l)
		k.mentionList.Append(row)
	}
	if len(hits) > 8 {
		hits = hits[:8]
	}
	k.hits, k.query = hits, query
	k.mentionList.SelectRow(k.mentionList.RowAtIndex(0))
	k.mentionPop.Popup()
	k.view.GrabFocus() // keep typing and arrow keys in the composer's key controller
}

// pickMention replaces the typed "@query" with the chosen member's name.
func (k *Composer) pickMention(i int) {
	if i < 0 || i >= len(k.hits) {
		return
	}
	p := k.hits[i]
	buf := k.view.Buffer()
	cursor := buf.IterAtMark(buf.GetInsert())
	start := cursor.Copy()
	start.BackwardChars(len([]rune(k.query)) + 1)
	k.mentionPop.Popdown()
	k.hits = nil
	buf.Delete(start, cursor)
	label := "@" + p.name
	k.mentions[label] = p.jid
	buf.InsertAtCursor(label + " ")
}

// resolveMentions swaps each "@Name" picked from the list for "@number",
// which is how WhatsApp writes a tag; every client shows the name back.
func (k *Composer) resolveMentions(text string) (string, []string) {
	labels := make([]string, 0, len(k.mentions))
	for label := range k.mentions {
		labels = append(labels, label)
	}
	// longest first, so "@Ann" never rewrites the start of "@Anna"
	sort.Slice(labels, func(a, b int) bool { return len(labels[a]) > len(labels[b]) })
	var jids []string
	for _, label := range labels {
		if jid := k.mentions[label]; strings.Contains(text, label) {
			text = strings.ReplaceAll(text, label, "@"+strings.SplitN(jid, "@", 2)[0])
			jids = append(jids, jid)
		}
	}
	return text, jids
}

func contains(s, sub string) bool { return strings.Contains(strings.ToLower(s), strings.ToLower(sub)) }

// --- attachments -------------------------------------------------------------

func (k *Composer) attachButton() gtk.Widgetter {
	btn := gtk.NewMenuButton()
	btn.SetIconName("mail-attachment-symbolic")
	btn.SetTooltipText("Attach")
	btn.AddCSSClass("flat")
	btn.SetVAlign(gtk.AlignEnd)
	pop := gtk.NewPopover()
	col := gtk.NewBox(gtk.OrientationVertical, 2)
	add := func(label string, f func()) {
		b := gtk.NewButtonWithLabel(label)
		b.AddCSSClass("flat")
		b.ConnectClicked(func() { pop.Popdown(); f() })
		col.Append(b)
	}
	add("Photos, videos and files…", func() { k.pickFiles(false) })
	add("Send as document (full quality)…", func() { k.pickFiles(true) })
	add("Poll…", k.newPoll)
	pop.SetChild(col)
	btn.SetPopover(pop)
	return btn
}

func (k *Composer) pickFiles(asDocument bool) {
	d := gtk.NewFileDialog()
	d.SetTitle("Send files")
	d.OpenMultiple(context.Background(), &k.c.w.win.Window, func(res gio.AsyncResulter) {
		files, err := d.OpenMultipleFinish(res)
		if err != nil {
			return
		}
		var paths []string
		for i := uint(0); i < files.NItems(); i++ {
			paths = append(paths, files.Item(i).Cast().(*gio.File).Path())
		}
		k.confirmSend(paths, asDocument)
	})
}

// sendFile is the path for drag and drop.
func (k *Composer) sendFile(path string) { k.confirmSend([]string{path}, false) }

// confirmSend shows what is about to go out and takes an optional caption,
// as WhatsApp does, so a stray paste or drop never sends by itself.
func (k *Composer) confirmSend(paths []string, asDocument bool) {
	if len(paths) == 0 {
		return
	}
	chat := k.c.chat
	names := make([]string, len(paths))
	for i, p := range paths {
		names[i] = filepath.Base(p)
	}
	d := adw.NewAlertDialog(fmt.Sprintf("Send to %s?", k.c.w.c.Name(chat)), strings.Join(names, "\n"))
	box := gtk.NewBox(gtk.OrientationVertical, 8)
	if len(paths) == 1 {
		pic := gtk.NewPicture()
		pic.SetSizeRequest(280, 200)
		pic.SetContentFit(gtk.ContentFitContain)
		texture(paths[0], 560, func(t *gdk.MemoryTexture) { pic.SetPaintable(t) })
		box.Append(pic)
	}
	caption := gtk.NewEntry()
	caption.SetPlaceholderText("Add a caption")
	box.Append(caption)
	d.SetExtraChild(box)
	d.AddResponse("cancel", "Cancel")
	d.AddResponse("send", "Send")
	d.SetResponseAppearance("send", adw.ResponseSuggested)
	d.SetDefaultResponse("send")
	caption.SetActivatesDefault(true)
	reply := k.reply
	d.ConnectResponse(func(r string) {
		if r != "send" {
			return
		}
		for i, p := range paths {
			text := ""
			if i == 0 {
				text = caption.Text()
			}
			k.c.w.c.SendFile(chat, p, text, reply, asDocument)
			reply = nil
		}
		k.reply = nil
		k.banner.SetVisible(false)
	})
	d.Present(k.c.w.win)
}

// paste sends pasted files or images; plain text pastes as usual.
func (k *Composer) paste() bool {
	cb := k.view.Clipboard()
	formats := cb.Formats()
	switch {
	case formats.ContainGType(gdk.GTypeFileList):
		cb.ReadValueAsync(context.Background(), gdk.GTypeFileList, 0, func(res gio.AsyncResulter) {
			v, err := cb.ReadValueFinish(res)
			if err != nil {
				return
			}
			list, ok := v.GoValue().(*gdk.FileList)
			if !ok {
				return
			}
			var paths []string
			for _, f := range list.Files() {
				if p := f.Path(); p != "" {
					paths = append(paths, p)
				}
			}
			k.confirmSend(paths, false)
		})
		return true
	case formats.ContainGType(gdk.GTypeTexture) && !formats.ContainMIMEType("text/plain"):
		cb.ReadTextureAsync(context.Background(), func(res gio.AsyncResulter) {
			tex, err := cb.ReadTextureFinish(res)
			if err != nil || tex == nil {
				return
			}
			path := filepath.Join(os.TempDir(), fmt.Sprintf("Pasted image %s.png", time.Now().Format("2006-01-02 15.04.05")))
			if !gdk.BaseTexture(tex).SaveToPNG(path) {
				k.c.w.toast("Could not read the pasted image")
				return
			}
			k.confirmSend([]string{path}, false)
		})
		return true
	}
	return false
}

func (k *Composer) newPoll() {
	d := adw.NewAlertDialog("New poll", "")
	question := gtk.NewEntry()
	question.SetPlaceholderText("Question")
	options := gtk.NewTextView()
	options.AddCSSClass("card")
	options.SetTopMargin(6)
	options.SetLeftMargin(6)
	options.SetSizeRequest(300, 120)
	hint := gtk.NewLabel("One option per line, up to 12")
	hint.AddCSSClass("dim-label")
	hint.SetXAlign(0)
	multi := gtk.NewCheckButtonWithLabel("Allow multiple answers")
	multi.SetActive(true)
	box := gtk.NewBox(gtk.OrientationVertical, 8)
	box.Append(question)
	box.Append(hint)
	box.Append(options)
	box.Append(multi)
	d.SetExtraChild(box)
	d.AddResponse("cancel", "Cancel")
	d.AddResponse("send", "Send")
	d.SetResponseAppearance("send", adw.ResponseSuggested)
	chat := k.c.chat
	d.ConnectResponse(func(r string) {
		if r != "send" {
			return
		}
		start, end := options.Buffer().Bounds()
		var opts []string
		seen := map[string]bool{}
		for _, o := range strings.Split(options.Buffer().Text(start, end, false), "\n") {
			if o = strings.TrimSpace(o); o != "" && !seen[o] && len(opts) < 12 {
				opts, seen[o] = append(opts, o), true
			}
		}
		q := strings.TrimSpace(question.Text())
		if q == "" || len(opts) < 2 {
			k.c.w.toast("A poll needs a question and at least two options")
			return
		}
		k.c.w.c.SendPoll(chat, q, opts, multi.Active())
	})
	d.Present(k.c.w.win)
}

// --- stickers and emoji -----------------------------------------------------

func (k *Composer) stickerButton() gtk.Widgetter {
	btn := gtk.NewMenuButton()
	btn.SetIconName("face-smile-big-symbolic")
	btn.SetTooltipText("Stickers")
	btn.AddCSSClass("flat")
	btn.SetVAlign(gtk.AlignEnd)
	pop := gtk.NewPopover()
	grid := gtk.NewFlowBox()
	grid.SetMaxChildrenPerLine(5)
	grid.SetSelectionMode(gtk.SelectionNone)
	scroll := gtk.NewScrolledWindow()
	scroll.SetSizeRequest(440, 360)
	scroll.SetChild(grid)
	pop.SetChild(scroll)
	btn.SetPopover(pop)
	var stickers []*store.Message
	pop.ConnectShow(func() {
		grid.RemoveAll()
		stickers, _ = k.c.w.c.Store.Stickers(100)
		if len(stickers) == 0 {
			grid.Append(italic("Stickers you receive show up here."))
		}
		for _, s := range stickers {
			pic := gtk.NewPicture()
			pic.SetSizeRequest(72, 72)
			pic.SetContentFit(gtk.ContentFitContain)
			texture(s.Media, 144, func(t *gdk.MemoryTexture) { pic.SetPaintable(t) })
			grid.Append(pic)
		}
	})
	grid.ConnectChildActivated(func(child *gtk.FlowBoxChild) {
		i := child.Index()
		if i < 0 || i >= len(stickers) {
			return
		}
		pop.Popdown()
		if err := k.c.w.c.SendSticker(k.c.chat, stickers[i], k.reply); err != nil {
			k.c.w.toast("Could not send sticker: " + err.Error())
		}
		k.reply = nil
		k.banner.SetVisible(false)
	})
	return btn
}

func (k *Composer) emojiButton() gtk.Widgetter {
	btn := gtk.NewMenuButton()
	btn.SetIconName("emoji-people-symbolic")
	btn.SetTooltipText("Emoji")
	btn.AddCSSClass("flat")
	btn.SetVAlign(gtk.AlignEnd)
	chooser := gtk.NewEmojiChooser()
	chooser.ConnectEmojiPicked(func(e string) {
		k.view.Buffer().InsertAtCursor(e)
		k.focus()
	})
	btn.SetPopover(&chooser.Popover)
	return btn
}

// --- voice notes ----------------------------------------------------------

func (k *Composer) recordingBar() gtk.Widgetter {
	k.recBox = gtk.NewBox(gtk.OrientationHorizontal, 8)
	k.recBox.SetVisible(false)
	dot := gtk.NewLabel("")
	dot.SetMarkup(`<span foreground="#e01b24">●</span>`)
	k.recLabel = gtk.NewLabel("0:00")
	k.recLabel.SetHExpand(true)
	k.recLabel.SetXAlign(0)
	cancel := gtk.NewButtonFromIconName("user-trash-symbolic")
	cancel.SetTooltipText("Discard (Esc)")
	cancel.AddCSSClass("flat")
	cancel.ConnectClicked(k.cancelRecording)
	stop := gtk.NewButtonFromIconName("paper-plane-symbolic")
	stop.SetTooltipText("Send voice message")
	stop.AddCSSClass("suggested-action")
	stop.AddCSSClass("circular")
	stop.ConnectClicked(k.finishRecording)
	k.recBox.Append(dot)
	k.recBox.Append(k.recLabel)
	k.recBox.Append(cancel)
	k.recBox.Append(stop)
	return k.recBox
}

// startRecording records the default PipeWire source straight to Ogg/Opus,
// the format WhatsApp voice notes use, so nothing needs converting after.
func (k *Composer) startRecording() {
	k.recPath = filepath.Join(os.TempDir(), fmt.Sprintf("linwa-voice-%d.ogg", time.Now().UnixNano()))
	k.rec = exec.Command("gst-launch-1.0", "-q", "-e", "pipewiresrc", "!", "audioconvert", "!",
		"audioresample", "!", "audio/x-raw,rate=48000,channels=1", "!", "opusenc", "bitrate=32000", "!",
		"oggmux", "!", "filesink", "location="+k.recPath)
	if err := k.rec.Start(); err != nil {
		k.rec = nil
		k.c.w.toast("Could not start recording: " + err.Error())
		return
	}
	k.recStart = time.Now()
	k.recBox.SetVisible(true)
	k.changed()
	glib.TimeoutAdd(250, func() bool {
		if k.rec == nil {
			return false
		}
		s := int(time.Since(k.recStart).Seconds())
		k.recLabel.SetText(fmt.Sprintf("%d:%02d", s/60, s%60))
		return true
	})
}

func (k *Composer) stopRecorder(keep bool) (string, bool) {
	if k.rec == nil {
		return "", false
	}
	cmd, path := k.rec, k.recPath
	long := time.Since(k.recStart) >= time.Second
	k.rec = nil
	k.recBox.SetVisible(false)
	k.changed()
	if !keep || !long {
		_ = cmd.Process.Kill()
		go func() { _ = cmd.Wait(); os.Remove(path) }()
		return "", false
	}
	_ = cmd.Process.Signal(os.Interrupt) // -e: finish the Ogg file properly
	return path, true
}

func (k *Composer) cancelRecording() { k.stopRecorder(false) }

func (k *Composer) finishRecording() {
	cmd := k.rec
	path, ok := k.stopRecorder(true)
	if !ok {
		return
	}
	chat, reply := k.c.chat, k.reply
	k.reply = nil
	k.banner.SetVisible(false)
	go func() {
		_ = cmd.Wait()
		k.c.w.c.SendVoice(chat, path, reply)
	}()
}
