package files

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BackupKeep — сколько последних дампов держать в Drive (локально ротацией
// занимается контейнер бэкапов).
const BackupKeep = 30

// UploadBackup кладёт в Drive (LifeTask/Бэкапы) самый свежий дамп из dir, если
// его там ещё нет, и убирает в корзину дампы сверх BackupKeep. Возвращает имя
// загруженного файла или "".
func (s *Store) UploadBackup(ctx context.Context, dir string) (string, error) {
	path, err := newestDump(dir)
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
	// Список идёт от старых к новым — лишние в начале.
	for i := 0; i < len(have)-BackupKeep; i++ {
		if err := s.Drive.DriveTrash(ctx, have[i].ID); err != nil {
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

// newestDump — самый свежий *.sql.gz в dir (рекурсивно). «latest»-ссылки
// пропускаем: у них одно имя на все дни.
func newestDump(dir string) (string, error) {
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
		if d.Type()&fs.ModeSymlink != 0 || d.IsDir() || !strings.HasSuffix(name, ".sql.gz") || strings.Contains(name, "latest") {
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
	sort.Slice(dumps, func(i, j int) bool { return dumps[i].mod.After(dumps[j].mod) })
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
