// Package files — вложения задач. Файл лежит в Google Drive (папка LifeTask),
// а если Drive не подключён или недоступен — на диске сервера. В базе только
// блок body {"type":"file","file_id":"d:…|l:…","name","mime","size"}: префикс
// id говорит, где искать, поэтому старые локальные файлы открываются и после
// подключения Drive.
package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

// MaxSize — предел одного файла. Telegram боту и так больше 20 МБ не отдаёт.
const MaxSize = 50 << 20

var ErrNotFound = errors.New("файл не найден")

// Drive — то, что нужно от Google Drive (gcal.Google; в тестах — фейк).
type Drive interface {
	DriveFolder(ctx context.Context, name, parent string) (string, error)
	DriveUpload(ctx context.Context, folder, name, mime string, data []byte) (string, error)
	DriveOpen(ctx context.Context, id string) (io.ReadCloser, error)
	DriveList(ctx context.Context, folder string) ([]DriveFileInfo, error)
	DriveTrash(ctx context.Context, id string) error
}

// DriveFileInfo — копия gcal.DriveFile, чтобы files не зависел от gcal.
type DriveFileInfo struct {
	ID, Name string
}

const (
	RootFolder        = "LifeTask"
	AttachmentsFolder = "Вложения"
	BackupsFolder     = "Бэкапы"
)

type Store struct {
	Dir   string // локальные файлы
	Drive Drive  // nil — только локально

	mu      sync.Mutex
	folders map[string]string
}

func New(dir string, d Drive) *Store { return &Store{Dir: dir, Drive: d, folders: map[string]string{}} }

// folder — id папки LifeTask/<name> в Drive (создаётся при первом обращении).
func (s *Store) folder(ctx context.Context, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.folders[name]; ok {
		return id, nil
	}
	root, ok := s.folders[""]
	if !ok {
		var err error
		if root, err = s.Drive.DriveFolder(ctx, RootFolder, ""); err != nil {
			return "", err
		}
		s.folders[""] = root
	}
	id, err := s.Drive.DriveFolder(ctx, name, root)
	if err != nil {
		return "", err
	}
	s.folders[name] = id
	return id, nil
}

// Put сохраняет файл и возвращает блок для body. Drive не ответил — файл всё
// равно не теряется: ложится на диск.
func (s *Store) Put(ctx context.Context, name, mimeType string, data []byte) (domain.Block, error) {
	if len(data) == 0 {
		return nil, errors.New("пустой файл")
	}
	if len(data) > MaxSize {
		return nil, fmt.Errorf("файл больше %d МБ", MaxSize>>20)
	}
	name = CleanName(name)
	mimeType = DetectMime(name, mimeType, data)
	block := domain.Block{"type": "file", "name": name, "mime": mimeType, "size": len(data)}
	if s.Drive != nil {
		id, err := s.putDrive(ctx, name, mimeType, data)
		if err == nil {
			block["file_id"] = "d:" + id
			return block, nil
		}
		slog.Warn("drive: не загрузил, сохраняю на диск", "err", err)
	}
	id, err := s.putLocal(data)
	if err != nil {
		return nil, err
	}
	block["file_id"] = "l:" + id
	return block, nil
}

func (s *Store) putDrive(ctx context.Context, name, mimeType string, data []byte) (string, error) {
	folder, err := s.folder(ctx, AttachmentsFolder)
	if err != nil {
		return "", err
	}
	return s.Drive.DriveUpload(ctx, folder, name, mimeType, data)
}

var localID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *Store) putLocal(data []byte) (string, error) {
	if s.Dir == "" {
		return "", errors.New("некуда сохранить файл: нет ни Google Drive, ни FILES_DIR")
	}
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return "", err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	tmp := filepath.Join(s.Dir, id+".tmp")
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return "", err
	}
	return id, os.Rename(tmp, filepath.Join(s.Dir, id))
}

// Open — содержимое по file_id из блока.
func (s *Store) Open(ctx context.Context, fileID string) (io.ReadCloser, error) {
	kind, id, ok := strings.Cut(fileID, ":")
	if !ok || id == "" {
		return nil, ErrNotFound
	}
	switch kind {
	case "d":
		if s.Drive == nil {
			return nil, errors.New("файл в Google Drive, а Drive не подключён")
		}
		return s.Drive.DriveOpen(ctx, id)
	case "l":
		if !localID.MatchString(id) {
			return nil, ErrNotFound
		}
		f, err := os.Open(filepath.Join(s.Dir, id))
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return f, err
	}
	return nil, ErrNotFound
}

// CleanName — имя без пути и управляющих символов, не длиннее 120 символов.
func CleanName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == "/" {
		name = "file"
	}
	if utf8.RuneCountInString(name) > 120 {
		ext := filepath.Ext(name)
		if utf8.RuneCountInString(ext) > 10 {
			ext = ""
		}
		r := []rune(strings.TrimSuffix(name, ext))
		name = string(r[:120-utf8.RuneCountInString(ext)]) + ext
	}
	return name
}

// DetectMime — тип из заявленного, по расширению или по содержимому.
func DetectMime(name, declared string, data []byte) string {
	if declared != "" && declared != "application/octet-stream" {
		if t, _, err := mime.ParseMediaType(declared); err == nil {
			return t
		}
	}
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); t != "" {
		t, _, _ = mime.ParseMediaType(t)
		return t
	}
	t, _, _ := mime.ParseMediaType(http.DetectContentType(data[:min(len(data), 512)]))
	return t
}

// Inline — можно ли показывать файл прямо в браузере. Остальное (в т.ч. HTML и
// SVG, в которых бывают скрипты) только скачивается.
func Inline(mimeType string) bool {
	switch mimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/heic", "application/pdf",
		"text/plain", "audio/ogg", "audio/mpeg", "audio/mp4", "video/mp4", "video/quicktime":
		return true
	}
	return false
}

// ReadAll читает не больше MaxSize: больше — ошибка, а не обрезанный файл.
func ReadAll(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, MaxSize+1))
	if err != nil {
		return nil, err
	}
	if n > MaxSize {
		return nil, fmt.Errorf("файл больше %d МБ", MaxSize>>20)
	}
	return buf.Bytes(), nil
}
