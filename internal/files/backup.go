package files

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BackupKeep — сколько последних копий каждого вида держать в Drive.
const BackupKeep = 30

// UploadBackup кладёт в Drive (LifeTask/Бэкапы) свежий дамп БД и архив локальных
// вложений. Ротация независима: по BackupKeep копий каждого вида. Возвращает
// имена загруженных файлов через запятую или "". Сбой одного вида не мешает другому.
func (s *Store) UploadBackup(ctx context.Context, dir string) (string, error) {
	if s.Drive == nil {
		return "", nil
	}
	var uploaded []string
	var errs []error
	for _, match := range []func(string) bool{isDump, isFilesBackup} {
		name, err := s.uploadBackupKind(ctx, dir, match)
		if name != "" {
			uploaded = append(uploaded, name)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return strings.Join(uploaded, ", "), errors.Join(errs...)
}

func isDump(name string) bool { return strings.HasSuffix(name, ".sql.gz") }

func isFilesBackup(name string) bool {
	return strings.HasPrefix(name, "files-") && strings.HasSuffix(name, ".tar.gz")
}

func (s *Store) uploadBackupKind(ctx context.Context, dir string, match func(string) bool) (string, error) {
	path, err := newestBackup(dir, match)
	if err != nil || path == "" {
		return "", err
	}
	folder, err := s.folder(ctx, BackupsFolder)
	if err != nil {
		return "", err
	}
	have, err := s.Drive.DriveList(ctx, folder)
	if err != nil {
		return "", err
	}
	name := filepath.Base(path)
	uploaded := ""
	if !hasName(have, name) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		id, err := s.Drive.DriveUpload(ctx, folder, name, "application/gzip", data)
		if err != nil {
			return "", err
		}
		have = append(have, DriveFileInfo{ID: id, Name: name})
		uploaded = name
	}
	// DriveList идёт от старых к новым; посторонние файлы не трогаем.
	var kind []DriveFileInfo
	for _, f := range have {
		if match(f.Name) {
			kind = append(kind, f)
		}
	}
	for i := 0; i < len(kind)-BackupKeep; i++ {
		if err := s.Drive.DriveTrash(ctx, kind[i].ID); err != nil {
			return uploaded, err
		}
	}
	return uploaded, nil
}

// RunBackups — раз в несколько часов: дамп делается раз в сутки, а повторная
// проверка ничего не стоит и переживает перезапуски.
func (s *Store) RunBackups(ctx context.Context, dir string, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		name, err := s.UploadBackup(ctx, dir)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.Error("бэкап в Drive", "err", err)
		case name != "":
			slog.Info("бэкап в Drive", "file", name)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// newestBackup — самая свежая опубликованная копия в dir (рекурсивно).
// Временные файлы и «latest»-ссылки пропускаем.
func newestBackup(dir string, match func(string) bool) (string, error) {
	type dump struct {
		path string
		mod  time.Time
	}
	var dumps []dump
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil
		}
		name := d.Name()
		if d.Type()&fs.ModeSymlink != 0 || d.IsDir() || !match(name) || strings.Contains(name, "latest") {
			return nil
		}
		info, err := d.Info()
		if err == nil && info.Size() > 0 {
			dumps = append(dumps, dump{p, info.ModTime()})
		}
		return nil
	})
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil || len(dumps) == 0 {
		return "", err
	}
	sort.Slice(dumps, func(i, j int) bool {
		if dumps[i].mod.Equal(dumps[j].mod) {
			return dumps[i].path > dumps[j].path
		}
		return dumps[i].mod.After(dumps[j].mod)
	})
	return dumps[0].path, nil
}

func hasName(list []DriveFileInfo, name string) bool {
	for _, f := range list {
		if f.Name == name {
			return true
		}
	}
	return false
}
