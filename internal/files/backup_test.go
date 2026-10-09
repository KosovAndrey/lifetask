package files

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupKindsAndRotation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d := newFakeDrive()
	s := New("", d)
	folder, err := s.folder(ctx, BackupsFolder)
	if err != nil {
		t.Fatal(err)
	}
	otherID, _ := d.DriveUpload(ctx, folder, "restore-notes.txt", "text/plain", []byte("keep"))
	now := time.Now()
	write := func(name string, age time.Duration, data []byte) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(age), now.Add(age)); err != nil {
			t.Fatal(err)
		}
	}
	// Более новые неопубликованные/пустые файлы и ссылки нельзя загрузить.
	write("files-incomplete.tar.gz.tmp", time.Hour, []byte("partial"))
	write("files-empty.tar.gz", time.Hour, nil)
	write("files-latest.tar.gz", time.Hour, []byte("latest"))
	if err := os.Symlink(filepath.Join(dir, "files-latest.tar.gz"), filepath.Join(dir, "files-link.tar.gz")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < BackupKeep+2; i++ {
		dump := fmt.Sprintf("lifeplan-%02d.sql.gz", i)
		archive := fmt.Sprintf("files-%02d.tar.gz", i)
		write(dump, time.Duration(i)*time.Minute, []byte("db"))
		write(archive, time.Duration(i)*time.Minute, []byte("files"))
		name, err := s.UploadBackup(ctx, dir)
		if err != nil || name != dump+", "+archive {
			t.Fatalf("upload %d: %q, %v", i, name, err)
		}
	}
	if name, err := s.UploadBackup(ctx, dir); err != nil || name != "" {
		t.Fatalf("повторная загрузка: %q %v", name, err)
	}
	list, _ := d.DriveList(ctx, folder)
	dumps, archives := 0, 0
	for _, f := range list {
		if isDump(f.Name) {
			dumps++
		} else if isFilesBackup(f.Name) {
			archives++
		}
	}
	if dumps != BackupKeep || archives != BackupKeep || len(d.trashed) != 4 {
		t.Fatalf("rotation: dumps=%d archives=%d trashed=%d", dumps, archives, len(d.trashed))
	}
	if _, ok := d.files[otherID]; !ok {
		t.Fatal("ротация удалила посторонний файл")
	}
}

type failingDumpDrive struct{ *fakeDrive }

func (d failingDumpDrive) DriveUpload(ctx context.Context, folder, name, mime string, data []byte) (string, error) {
	if strings.HasSuffix(name, ".sql.gz") {
		return "", errors.New("dump upload failed")
	}
	return d.fakeDrive.DriveUpload(ctx, folder, name, mime, data)
}

func TestBackupKindsFailIndependently(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"lifeplan.sql.gz", "files-20261009.tar.gz"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("backup"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	d := failingDumpDrive{newFakeDrive()}
	s := New("", d)
	name, err := s.UploadBackup(context.Background(), dir)
	if name != "files-20261009.tar.gz" || err == nil {
		t.Fatalf("успех вложений при сбое БД: %q %v", name, err)
	}
	if name, err := New("", nil).UploadBackup(context.Background(), dir); name != "" || err != nil {
		t.Fatalf("без Drive: %q %v", name, err)
	}
}
