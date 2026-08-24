package sniff

import (
	"bytes"
	"strings"
)

// Detect returns the MIME type of data by inspecting leading magic bytes.
// Unlike http.DetectContentType, it correctly identifies SVG, WebP, and the
// MP4/WebM/Ogg container formats. Unknown content yields application/octet-stream.
func Detect(data []byte) string {
	n := len(data)
	if n == 0 {
		return "application/octet-stream"
	}

	// Raster images
	switch {
	case n >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "image/png"
	case n >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg"
	case n >= 6 && (bytes.Equal(data[:6], []byte("GIF87a")) || bytes.Equal(data[:6], []byte("GIF89a"))):
		return "image/gif"
	case n >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	case n >= 2 && data[0] == 'B' && data[1] == 'M':
		return "image/bmp"
	case n >= 4 && ((data[0] == 'I' && data[1] == 'I' && data[2] == 0x2A && data[3] == 0x00) ||
		(data[0] == 'M' && data[1] == 'M' && data[2] == 0x00 && data[3] == 0x2A)):
		return "image/tiff"
	}

	// SVG is XML-based; sniff before any generic text classification.
	if looksLikeSVG(data) {
		return "image/svg+xml"
	}

	// Video and audio containers
	switch {
	case n >= 12 && bytes.Equal(data[4:8], []byte("ftyp")):
		return "video/mp4"
	case n >= 4 && data[0] == 0x1A && data[1] == 0x45 && data[2] == 0xDF && data[3] == 0xA3:
		return "video/webm"
	case n >= 4 && bytes.Equal(data[:4], []byte("OggS")):
		return "video/ogg"
	case n >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WAVE")):
		return "audio/wav"
	case n >= 3 && bytes.Equal(data[:3], []byte("ID3")):
		return "audio/mpeg"
	case n >= 2 && data[0] == 0xFF && (data[1]&0xE0) == 0xE0:
		return "audio/mpeg"
	case n >= 5 && bytes.Equal(data[:5], []byte("%PDF-")):
		return "application/pdf"
	}

	return "application/octet-stream"
}

// looksLikeSVG reports whether data begins with an <svg> root or an XML
// declaration immediately followed by an <svg> element.
func looksLikeSVG(data []byte) bool {
	if len(data) > 1024 {
		data = data[:1024]
	}

	s := bytes.TrimLeft(data, " \t\r\n\xef\xbb\xbf")
	lower := strings.ToLower(string(s))

	if strings.HasPrefix(lower, "<svg") {
		return true
	}
	if strings.HasPrefix(lower, "<?xml") {
		return strings.Contains(lower, "<svg")
	}
	return false
}
