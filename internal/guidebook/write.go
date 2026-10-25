package guidebook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WriteOutcome says what one write did, so the caller can tell somebody rather
// than leaving them to look.
type WriteOutcome struct {
	// Path is repository-relative: FileName or DraftFileName.
	Path string
	// Draft is true when FileName already existed and was left untouched.
	Draft bool
	// ReplacedDraft is true when an earlier draft was overwritten.
	ReplacedDraft bool
}

// Write puts the guidebook at the project root without ever overwriting a
// confirmed one. Issue #22's "running it twice is safe", decided in favor of
// never clobbering:
//
//   - No INSPECTOR-GUIDEBOOK.md yet: it is created, unconfirmed.
//   - One already there: it is left exactly as it is and the new document goes
//     to INSPECTOR-GUIDEBOOK.draft.md beside it, for a person to diff. That
//     file may well have been read, corrected and signed by hand, and a
//     generator that discards a human's confirmation has taught everyone to
//     stop confirming.
//   - A draft already there: it is replaced, and the caller is told so. A draft
//     is inspector's own output and the thing a person is expected to read and
//     then move into place; refusing on the second run would leave a project
//     stuck after a single abandoned review, which is the opposite of safe to
//     run twice.
//
// The real file is created with O_EXCL rather than after a stat, so two runs at
// once cannot both decide the file is absent.
func Write(root string, content []byte) (WriteOutcome, error) {
	realPath := filepath.Join(root, FileName)
	file, err := os.OpenFile(realPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	switch {
	case err == nil:
		if err := writeAndClose(file, content); err != nil {
			return WriteOutcome{}, err
		}
		return WriteOutcome{Path: FileName}, nil
	case !errors.Is(err, os.ErrExist):
		return WriteOutcome{}, fmt.Errorf("writing %s: %w", FileName, err)
	}

	draftPath := filepath.Join(root, DraftFileName)
	_, statErr := os.Lstat(draftPath)
	replaced := statErr == nil
	if err := os.WriteFile(draftPath, content, 0o644); err != nil {
		return WriteOutcome{}, fmt.Errorf("writing %s: %w", DraftFileName, err)
	}
	return WriteOutcome{Path: DraftFileName, Draft: true, ReplacedDraft: replaced}, nil
}

func writeAndClose(file *os.File, content []byte) error {
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return fmt.Errorf("writing %s: %w", FileName, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", FileName, err)
	}
	return nil
}

// Describe is what the caller prints about where the guidebook went. The draft
// case says what to do next, because a person who is not told they have two
// files will read the stale one.
func (o WriteOutcome) Describe() string {
	if !o.Draft {
		return fmt.Sprintf("wrote %s - unconfirmed until somebody dates and signs its confirmed line", o.Path)
	}
	if o.ReplacedDraft {
		return fmt.Sprintf("%s already exists and was left untouched; this run replaced the earlier %s beside it - diff the two, then move what you confirm into place", FileName, o.Path)
	}
	return fmt.Sprintf("%s already exists and was left untouched; this run wrote %s beside it - diff the two, then move what you confirm into place", FileName, o.Path)
}
