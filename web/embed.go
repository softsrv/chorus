package web

import (
	"embed"
	"io/fs"
	"os"
)

//go:embed static
var embedded embed.FS

// Static returns the web client's files (index.html, js, css). Production
// serves the copy embedded in the binary; development reads from disk so edits
// to the client show up on reload without restarting the server.
func Static(fromDisk bool) fs.FS {
	if fromDisk {
		if _, err := os.Stat("web/static/index.html"); err == nil {
			return os.DirFS("web/static")
		}
	}
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		panic(err) // the embed pattern guarantees "static" exists
	}
	return sub
}
