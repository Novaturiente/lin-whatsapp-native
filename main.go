// lin-whatsapp-native: a WhatsApp chat client that talks the protocol
// directly (whatsmeow) and draws with GTK4/libadwaita. No browser engine.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"

	"lin-whatsapp-native/store"
	"lin-whatsapp-native/ui"
	"lin-whatsapp-native/wa"
)

const appID = "io.github.nova.LinWhatsappNative"

func dataDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".local", "share")
	}
	return filepath.Join(base, "lin-whatsapp-native")
}

func main() {
	dir := dataDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	st, err := store.Open(filepath.Join(dir, "chat.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "chat store:", err)
		os.Exit(1)
	}
	client, err := wa.Open(context.Background(), dir, st)
	if err != nil {
		fmt.Fprintln(os.Stderr, "whatsapp store:", err)
		os.Exit(1)
	}

	app := adw.NewApplication(appID, gio.ApplicationFlagsNone)
	var win *ui.Window
	app.ConnectActivate(func() {
		if win != nil { // a second launch just raises the window
			win.Present()
			return
		}
		win = ui.New(app, client)
		win.Present()
		if err := client.Connect(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "connect:", err)
		}
	})
	code := app.Run(os.Args)
	client.WA.Disconnect()
	st.Close()
	os.Exit(code)
}
