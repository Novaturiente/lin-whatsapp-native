// Package media holds the file helpers around WhatsApp media: names, types,
// previews and decoding stickers.
package media

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
	"google.golang.org/protobuf/proto"
)

// Ext picks a file extension: the original name's if there is one, else
// one for the MIME type.
func Ext(mimeType, name string) string {
	if e := filepath.Ext(name); e != "" {
		return e
	}
	base := strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0])
	switch base { // the system table answers oddly for these
	case "audio/ogg":
		return ".ogg"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4":
		return ".m4a"
	}
	if exts, _ := mime.ExtensionsByType(base); len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

// Type is a file's MIME type, from its extension or else its first bytes.
func Type(path string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); t != "" {
		return strings.SplitN(t, ";", 2)[0]
	}
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := f.Read(head)
	return strings.SplitN(http.DetectContentType(head[:n]), ";", 2)[0]
}

// Decode reads an image file, WebP stickers included.
func Decode(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

// Scaled decodes an image no larger than max pixels on its long side, as
// straight (non-premultiplied) RGBA. Chats are full of 12-megapixel photos;
// shown at full size, fifty of them would hold gigabytes.
func Scaled(path string, max int) (*image.NRGBA, error) {
	img, err := Decode(path)
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > max || h > max {
		if w >= h {
			w, h = max, max1(max*h/w)
		} else {
			w, h = max1(max*w/h), max
		}
	}
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.ApproxBiLinear.Scale(out, out.Bounds(), img, b, draw.Src, nil)
	return out, nil
}

// Thumbnail makes the small JPEG preview sent inside a media message, and
// returns the full image's size.
func Thumbnail(img image.Image, max int) (jpg []byte, w, h int) {
	b := img.Bounds()
	w, h = b.Dx(), b.Dy()
	tw, th := max, max*h/w
	if h > w {
		tw, th = max*w/h, max
	}
	small := image.NewRGBA(image.Rect(0, 0, max1(tw), max1(th)))
	draw.ApproxBiLinear.Scale(small, small.Bounds(), img, b, draw.Src, nil)
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, small, &jpeg.Options{Quality: 60})
	return buf.Bytes(), w, h
}

func max1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

// VideoThumbnail grabs a frame with ffmpeg and returns it as a preview JPEG,
// or nil when ffmpeg is missing or fails. The message still sends without it.
func VideoThumbnail(path string) []byte {
	out, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-frames:v", "1",
		"-vf", "scale=320:-2", "-f", "image2", "-c:v", "mjpeg", "-").Output()
	if err != nil {
		return nil
	}
	return out
}

// Size is the byte size a stored media message declares, 0 when unknown.
func Size(raw []byte) uint64 {
	var m waE2E.Message
	if len(raw) == 0 || proto.Unmarshal(raw, &m) != nil {
		return 0
	}
	for _, n := range []uint64{m.GetVideoMessage().GetFileLength(), m.GetImageMessage().GetFileLength(),
		m.GetAudioMessage().GetFileLength(), m.GetDocumentMessage().GetFileLength(),
		m.GetPtvMessage().GetFileLength(), m.GetStickerMessage().GetFileLength()} {
		if n > 0 {
			return n
		}
	}
	return 0
}

// Seconds is a media file's duration, through ffprobe; 0 when unknown.
func Seconds(path string) uint32 {
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		return 0
	}
	var s float64
	fmt.Sscan(string(out), &s)
	return uint32(s + 0.5)
}

// Unique returns a path in dir for name that does not exist yet:
// photo.jpg, then photo-1.jpg, photo-2.jpg.
func Unique(dir, name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	path := filepath.Join(dir, name)
	for n := 1; ; n++ {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, n, ext))
	}
}

// Copy copies a file, for saving media out of the app's own folder.
func Copy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

var web = &http.Client{Timeout: 30 * time.Second}

// Get fetches a plain URL, such as a profile picture.
func Get(url string) ([]byte, error) {
	resp, err := web.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 10<<20))
}
