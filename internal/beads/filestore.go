package beads

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gastownhall/gascity/internal/fsys"
)

// fileData is the on-disk JSON format for the bead store.
type fileData struct {
	Seq   int    `json:"seq"`
	Beads []Bead `json:"beads"`
	Deps  []Dep  `json:"deps,omitempty"`
}

// FileStore is a file-backed Store implementation. It embeds a MemStore for
// all bead logic and adds JSON persistence — load on open, flush on every
// write. Fine for Tutorial 01 volumes.
type FileStore struct {
	*MemStore
	fmu  sync.Mutex // guards mutate-then-save atomicity
	fs   fsys.FS
	path string
}

// OpenFileStore opens or creates a file-backed bead store at path. All file
// I/O goes through fs for testability. If the file exists, its contents are
// loaded into memory. If it doesn't exist, the store starts empty. Parent
// directories are created as needed.
func OpenFileStore(fs fsys.FS, path string) (*FileStore, error) {
	if err := fs.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("opening file store: %w", err)
	}

	data, err := fs.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &FileStore{MemStore: NewMemStore(), fs: fs, path: path}, nil
		}
		return nil, fmt.Errorf("opening file store: %w", err)
	}

	var fd fileData
	if err := json.Unmarshal(data, &fd); err != nil {
		return nil, fmt.Errorf("opening file store: %w", err)
	}
	return &FileStore{MemStore: NewMemStoreFrom(fd.Seq, fd.Beads, fd.Deps), fs: fs, path: path}, nil
}

// Create delegates to MemStore.Create and flushes to disk.
func (fs *FileStore) Create(b Bead) (Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	result, err := fs.MemStore.Create(b)
	if err != nil {
		return Bead{}, err
	}
	if err := fs.save(); err != nil {
		return Bead{}, err
	}
	return result, nil
}

// Update delegates to MemStore.Update and flushes to disk.
func (fs *FileStore) Update(id string, opts UpdateOpts) error {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.MemStore.Update(id, opts); err != nil {
		return err
	}
	return fs.save()
}

// Close delegates to MemStore.Close and flushes to disk.
func (fs *FileStore) Close(id string) error {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.MemStore.Close(id); err != nil {
		return err
	}
	return fs.save()
}

// MolCook delegates to MemStore.MolCook and flushes to disk.
func (fs *FileStore) MolCook(formula, title string, vars []string) (string, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	id, err := fs.MemStore.MolCook(formula, title, vars)
	if err != nil {
		return "", err
	}
	if err := fs.save(); err != nil {
		return "", err
	}
	return id, nil
}

// MolCookOn delegates to MemStore.MolCookOn and flushes to disk.
func (fs *FileStore) MolCookOn(formula, beadID, title string, vars []string) (string, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	id, err := fs.MemStore.MolCookOn(formula, beadID, title, vars)
	if err != nil {
		return "", err
	}
	if err := fs.save(); err != nil {
		return "", err
	}
	return id, nil
}

// SetMetadata delegates to MemStore.SetMetadata and flushes to disk.
func (fs *FileStore) SetMetadata(id, key, value string) error {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.MemStore.SetMetadata(id, key, value); err != nil {
		return err
	}
	return fs.save()
}

// DepAdd delegates to MemStore.DepAdd and flushes to disk.
func (fs *FileStore) DepAdd(issueID, dependsOnID, depType string) error {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.MemStore.DepAdd(issueID, dependsOnID, depType); err != nil {
		return err
	}
	return fs.save()
}

// DepRemove delegates to MemStore.DepRemove and flushes to disk.
func (fs *FileStore) DepRemove(issueID, dependsOnID string) error {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.MemStore.DepRemove(issueID, dependsOnID); err != nil {
		return err
	}
	return fs.save()
}

// save writes the full store state to disk atomically (temp file + rename).
// Called with fmu held, so snapshot under MemStore.mu then release before I/O.
func (fs *FileStore) save() error {
	fs.mu.Lock()
	seq, beads, deps := fs.snapshot()
	fs.mu.Unlock()

	fd := fileData{Seq: seq, Beads: beads, Deps: deps}
	data, err := json.MarshalIndent(fd, "", "  ")
	if err != nil {
		return fmt.Errorf("saving file store: %w", err)
	}

	tmp := fs.path + ".tmp"
	if err := fs.fs.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("saving file store: %w", err)
	}
	if err := fs.fs.Rename(tmp, fs.path); err != nil {
		return fmt.Errorf("saving file store: %w", err)
	}
	return nil
}
