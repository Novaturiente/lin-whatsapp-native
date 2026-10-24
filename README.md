# lin-whatsapp-native

A WhatsApp chat client for Linux with no browser inside. It speaks WhatsApp's
protocol directly through [whatsmeow](https://github.com/tulir/whatsmeow) and
draws with GTK4 and libadwaita. Chats only: no calls, status or channels.

whatsmeow is unofficial. Using it breaks WhatsApp's terms of service, and a
ban, while rare, is possible.

## Build

```sh
go build -o lin-whatsapp-native .
```

The first build compiles the GTK bindings and takes about 13 minutes. Later
builds take seconds. Runtime tools: `gst-launch-1.0` with `pipewiresrc` and
`opusenc` (voice notes), `ffmpeg`/`ffprobe` (video previews and durations),
`notify-send`.

## Run

```sh
./lin-whatsapp-native
```

Scan the QR code from your phone under Settings, Linked devices. The phone
then sends part of your history; older chats may be missing, as with any
newly linked device.

Install the launcher:

```sh
cp lin-whatsapp-native.desktop ~/.local/share/applications/
```

Data lives in `~/.local/share/lin-whatsapp-native/`: `wa.db` holds the
device keys (delete it to unlink), `chat.db` the messages, `media/` the
downloaded files.

## Chat features

- Text with WhatsApp formatting (`*bold*`, `_italic_`, `~strike~`, ```` ```mono``` ````), links, @mentions.
- Replies, reactions, edits (15 minutes), delete for everyone, forwarding.
- Photos, videos, GIFs, voice notes, audio, documents, stickers: shown inline, opened in their default app, or saved to `~/Downloads`.
- Sending files from the picker, by drag and drop, or by pasting (`Ctrl+V`) files or images. A preview with a caption box shows first.
- Sending stickers you have received, from the sticker tray.
- Recording voice notes.
- Polls: creating, voting and live results.
- Read receipts both ways, typing status, unread counts, desktop notifications.

Shown but not openable: view-once media (WhatsApp keeps it off linked devices),
contact cards, and message types whatsmeow does not know yet.

## Phone checklist: phase 1

- [ ] In a chat with disappearing messages on, the header shows ⏱, and a message sent from here disappears on the phone.
- [ ] A link sent from the phone shows its preview card here.
- [ ] A forwarded message shows "↪ Forwarded".
- [ ] Adding, removing or renaming in a group shows a grey line here.
- [ ] Calling this number shows "Incoming voice call" here and a notification.
- [ ] Turning Wi-Fi off shows the "Connecting…" banner; a message sent then shows "⚠ not sent", and "Retry sending" delivers it once back online.
- [ ] The ✎ button opens a contact by name, and a typed number starts a new chat.
- [ ] Scrolling to the top of an old chat loads older messages from the phone.

## Check

```sh
go vet ./... && go test ./...
```

`store`, `wa` (message conversion) and `markup` have tests. The GTK window has
none; test it by hand with a phone.
