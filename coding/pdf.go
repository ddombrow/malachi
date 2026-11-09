package coding

import (
	"io"
	"strings"

	"github.com/ledongthuc/pdf"
)

func openPDF(file io.ReaderAt, size int64) (*pdf.Reader, error) {
	return pdf.NewReader(file, size)
}

// pdfTextReader extracts one page at a time, so reading a large PDF does not
// keep all of its extracted text in memory.
type pdfTextReader struct {
	doc     *pdf.Reader
	page    int
	current io.Reader
	emitted bool
}

func newPDFTextReader(doc *pdf.Reader) io.Reader {
	return &pdfTextReader{doc: doc, page: 1}
}

func (r *pdfTextReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if r.current != nil {
			n, err := r.current.Read(p)
			if err != io.EOF || n > 0 {
				return n, err
			}
			r.current = nil
		}
		if r.page > r.doc.NumPage() {
			return 0, io.EOF
		}
		page := r.doc.Page(r.page)
		r.page++
		text, err := page.GetPlainText(nil)
		if err != nil {
			return 0, err
		}
		// PDF text extraction often adds boundary newlines. Remove only those so
		// leading spaces used for indentation survive.
		text = strings.Trim(text, "\r\n")
		if text == "" {
			continue
		}
		if r.emitted {
			text = "\n" + text
		}
		r.emitted = true
		r.current = strings.NewReader(text)
	}
}
