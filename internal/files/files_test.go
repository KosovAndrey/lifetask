package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDrive — Drive в памяти: папки и файлы с родителями.
type fakeDrive struct {
	fail    bool
	seq     int
	folders map[string]string // parent/name → id
	files   map[string][]byte
	names   map[string]string
	parent  map[string]string
	trashed []string
}

func newFakeDrive() *fakeDrive {
	return &fakeDrive{folders: map[string]string{}, files: map[string][]byte{}, names: map[string]string{}, parent: map[string]string{}}
}

func (f *fakeDrive) DriveFolder(_ context.Context, name, parent string) (string, error) {
	if f.fail {
		return "", errors.New("drive недоступен")
	}
	k := parent + "/" + name
	if id, ok := f.folders[k]; ok {
		return id, nil
	}
	f.seq++
	f.folders[k] = fmt.Sprintf("folder%d", f.seq)
	return f.folders[k], nil
}

func (f *fakeDrive) DriveUpload(_ context.Context, folder, name, _ string, data []byte) (string, error) {
	f.seq++
	id := fmt.Sprintf("file%d", f.seq)
	f.files[id], f.names[id], f.parent[id] = data, name, folder
	return id, nil
}

func (f *fakeDrive) DriveOpen(_ context.Context, id string) (io.ReadCloser, error) {
	data, ok := f.files[id]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeDrive) DriveList(_ context.Context, folder string) ([]DriveFileInfo, error) {
	var out []DriveFileInfo
	for i := 1; i <= f.seq; i++ { // по порядку создания, как orderBy=createdTime
		id := fmt.Sprintf("file%d", i)
		if _, ok := f.files[id]; ok && f.parent[id] == folder {
			out = append(out, DriveFileInfo{ID: id, Name: f.names[id]})
		}
	}
	return out, nil
}

func (f *fakeDrive) DriveTrash(_ context.Context, id string) error {
	f.trashed = append(f.trashed, id)
	delete(f.files, id)
	return nil
}

func read(t *testing.T, s *Store, id string) string {
	t.Helper()
	rc, err := s.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func TestLocalAndDrive(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	local := New(dir, nil)
	b, err := local.Put(ctx, "../../etc/passwd", "", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	lid := b["file_id"].(string)
	if !strings.HasPrefix(lid, "l:") || b["name"] != "passwd" || b["mime"] != "text/plain" {
		t.Fatalf("блок: %v", b)
	}
	if read(t, local, lid) != "hello" {
		t.Fatal("локальный файл не читается")
	}
	if _, err := local.Open(ctx, "l:../../x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("обход пути: %v", err)
	}

	d := newFakeDrive()
	s := New(dir, d)
	b2, err := s.Put(ctx, "скан.pdf", "application/pdf", []byte("%PDF"))
	if err != nil {
		t.Fatal(err)
	}
	did := b2["file_id"].(string)
	if !strings.HasPrefix(did, "d:") || read(t, s, did) != "%PDF" {
		t.Fatalf("drive: %v", b2)
	}
	// Файл лежит в LifeTask/Вложения.
	if d.parent[strings.TrimPrefix(did, "d:")] != d.folders[d.folders["/"+RootFolder]+"/"+AttachmentsFolder] {
		t.Fatal("не в папке вложений")
	}
	// Старый локальный открывается и с Drive.
	if read(t, s, lid) != "hello" {
		t.Fatal("локальный после подключения Drive")
	}
	// Drive упал — файл ложится на диск.
	d.fail = true
	s2 := New(dir, d)
	b3, err := s2.Put(ctx, "a.txt", "", []byte("x"))
	if err != nil || !strings.HasPrefix(b3["file_id"].(string), "l:") {
		t.Fatalf("запасной путь: %v %v", b3, err)
	}
	if _, err := s.Put(ctx, "big", "", make([]byte, MaxSize+1)); err == nil {
		t.Fatal("большой файл должен отклоняться")
	}
}

func TestCleanName(t *testing.T) {
	long := strings.Repeat("я", 200) + ".pdf"
	got := CleanName(long)
	if len([]rune(got)) != 120 || !strings.HasSuffix(got, ".pdf") {
		t.Fatalf("длинное имя: %d %q", len([]rune(got)), got[len(got)-8:])
	}
	if CleanName("a\x00\"b.txt") != "ab.txt" || CleanName("") != "file" {
		t.Fatal("чистка имени")
	}
}

func TestUploadBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d := newFakeDrive()
	s := New("", d)

	if name, err := s.UploadBackup(ctx, filepath.Join(dir, "nope")); err != nil || name != "" {
		t.Fatalf("нет каталога: %q %v", name, err)
	}
	daily := filepath.Join(dir, "daily")
	os.MkdirAll(daily, 0o755)
	os.MkdirAll(filepath.Join(dir, "last"), 0o755)
	now := time.Now()
	write := func(name string, age time.Duration) {
		p := filepath.Join(daily, name)
		os.WriteFile(p, []byte(name), 0o644)
		os.Chtimes(p, now.Add(-age), now.Add(-age))
	}
	write("lifeplan-20261002.sql.gz", 48*time.Hour)
	write("lifeplan-20261003.sql.gz", 24*time.Hour)
	os.WriteFile(filepath.Join(dir, "last", "lifeplan-latest.sql.gz"), []byte("latest"), 0o644)

	name, err := s.UploadBackup(ctx, dir)
	if err != nil || name != "lifeplan-20261003.sql.gz" {
		t.Fatalf("первый: %q %v", name, err)
	}
	if name, _ := s.UploadBackup(ctx, dir); name != "" {
		t.Fatal("повторно грузить не надо")
	}
	// Ротация: держим BackupKeep последних.
	for i := 0; i < BackupKeep+2; i++ {
		write(fmt.Sprintf("lifeplan-x%02d.sql.gz", i), -time.Duration(i+1)*time.Minute)
		if _, err := s.UploadBackup(ctx, dir); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := d.DriveList(ctx, d.folders[d.folders["/"+RootFolder]+"/"+BackupsFolder])
	if len(list) != BackupKeep || len(d.trashed) != 3 {
		t.Fatalf("в Drive %d, в корзине %d", len(list), len(d.trashed))
	}
}
