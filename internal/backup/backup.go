package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/oauth/accounts"
	"golang.org/x/crypto/argon2"
)

const (
	formatVersion  = 1
	maximumArchive = 512 << 20
)

var fileMagic = []byte("CRUX-BACKUP\x00")

var renameImportFile = os.Rename

type manifest struct {
	FormatVersion int      `json:"format_version"`
	CreatedAt     string   `json:"created_at"`
	Files         []string `json:"files"`
}

type Result struct {
	Files int
}

type source struct {
	archivePath string
	localPath   string
}

func Export(outputPath string, password []byte) (Result, error) {
	if len(password) == 0 {
		return Result{}, errors.New("password cannot be empty")
	}
	sources, err := exportSources()
	if err != nil {
		return Result{}, err
	}
	if len(sources) == 0 {
		return Result{}, errors.New("no provider, plugin, or account data found")
	}
	archive, err := buildArchive(sources)
	if err != nil {
		return Result{}, err
	}
	encrypted, err := encrypt(archive, password)
	if err != nil {
		return Result{}, err
	}
	if err := writeNewFile(outputPath, encrypted); err != nil {
		return Result{}, err
	}
	return Result{Files: len(sources)}, nil
}

func Import(inputPath string, password []byte) (Result, error) {
	if len(password) == 0 {
		return Result{}, errors.New("password cannot be empty")
	}
	info, err := os.Stat(inputPath)
	if err != nil {
		return Result{}, fmt.Errorf("inspect backup: %w", err)
	}
	if info.Size() > maximumArchive {
		return Result{}, fmt.Errorf("backup exceeds %d bytes", maximumArchive)
	}
	encrypted, err := os.ReadFile(inputPath)
	if err != nil {
		return Result{}, fmt.Errorf("read backup: %w", err)
	}
	archive, err := decrypt(encrypted, password)
	if err != nil {
		return Result{}, err
	}
	files, err := readArchive(archive)
	if err != nil {
		return Result{}, err
	}
	if err := restoreFiles(files); err != nil {
		return Result{}, err
	}
	return Result{Files: len(files)}, nil
}

func exportSources() ([]source, error) {
	accountPath, err := accounts.Path()
	if err != nil {
		return nil, fmt.Errorf("resolve account store: %w", err)
	}
	globalConfig := config.GlobalConfig()
	globalData := config.GlobalConfigData()
	candidates := []source{
		{archivePath: "global-config/crux.json", localPath: globalConfig},
		{archivePath: "global-config/cruxrc", localPath: filepath.Join(filepath.Dir(globalConfig), "cruxrc")},
		{archivePath: "global-data/crux.json", localPath: globalData},
		{archivePath: "global-data/connections.json", localPath: filepath.Join(config.GlobalWorkspaceDir(), "connections.json")},
		{archivePath: "accounts/accounts.json", localPath: accountPath},
		{archivePath: "global-data/plugin-state/trust.json", localPath: filepath.Join(config.GlobalWorkspaceDir(), "plugin-state", "trust.json")},
		{archivePath: "global-data/plugin-state/provenance.json", localPath: filepath.Join(config.GlobalWorkspaceDir(), "plugin-state", "provenance.json")},
	}
	result := make([]source, 0, len(candidates))
	for _, candidate := range candidates {
		info, err := os.Lstat(candidate.localPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", candidate.localPath, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("backup source is not a regular file: %s", candidate.localPath)
		}
		result = append(result, candidate)
	}
	pluginsRoot := filepath.Join(config.GlobalWorkspaceDir(), "plugins")
	err = filepath.WalkDir(pluginsRoot, func(localPath string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("plugin backup source is a symlink: %s", localPath)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("plugin backup source is not a regular file: %s", localPath)
		}
		relative, err := filepath.Rel(pluginsRoot, localPath)
		if err != nil {
			return err
		}
		result = append(result, source{
			archivePath: path.Join("global-data/plugins", filepath.ToSlash(relative)),
			localPath:   localPath,
		})
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("scan installed plugins: %w", err)
	}
	slices.SortFunc(result, func(a, b source) int {
		return strings.Compare(a.archivePath, b.archivePath)
	})
	return result, nil
}

func buildArchive(sources []source) ([]byte, error) {
	var output bytes.Buffer
	compressor := gzip.NewWriter(&output)
	archive := tar.NewWriter(compressor)
	files := make([]string, len(sources))
	for index, item := range sources {
		files[index] = item.archivePath
	}
	manifestData, err := json.Marshal(manifest{
		FormatVersion: formatVersion,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		Files:         files,
	})
	if err != nil {
		return nil, fmt.Errorf("encode backup manifest: %w", err)
	}
	if err := writeTarFile(archive, "manifest.json", manifestData); err != nil {
		return nil, err
	}
	for _, item := range sources {
		data, err := os.ReadFile(item.localPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", item.localPath, err)
		}
		if err := writeTarFile(archive, item.archivePath, data); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("close backup archive: %w", err)
	}
	if err := compressor.Close(); err != nil {
		return nil, fmt.Errorf("compress backup archive: %w", err)
	}
	return output.Bytes(), nil
}

func writeTarFile(archive *tar.Writer, name string, data []byte) error {
	header := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data))}
	if err := archive.WriteHeader(header); err != nil {
		return fmt.Errorf("write backup header %s: %w", name, err)
	}
	if _, err := archive.Write(data); err != nil {
		return fmt.Errorf("write backup data %s: %w", name, err)
	}
	return nil
}

func readArchive(data []byte) (map[string][]byte, error) {
	compressor, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open backup archive: %w", err)
	}
	defer compressor.Close()
	archive := tar.NewReader(compressor)
	files := make(map[string][]byte)
	var metadata manifest
	var totalSize int64
	manifestSeen := false
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read backup archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("unsupported backup entry: %s", header.Name)
		}
		if header.Size < 0 || header.Size > maximumArchive || totalSize+header.Size > maximumArchive {
			return nil, fmt.Errorf("backup entry is too large: %s", header.Name)
		}
		totalSize += header.Size
		content, err := io.ReadAll(io.LimitReader(archive, maximumArchive+1))
		if err != nil {
			return nil, fmt.Errorf("read backup entry %s: %w", header.Name, err)
		}
		if len(content) > maximumArchive {
			return nil, fmt.Errorf("backup entry is too large: %s", header.Name)
		}
		if header.Name == "manifest.json" {
			if manifestSeen {
				return nil, errors.New("duplicate backup manifest")
			}
			manifestSeen = true
			if err := json.Unmarshal(content, &metadata); err != nil {
				return nil, fmt.Errorf("decode backup manifest: %w", err)
			}
			continue
		}
		if _, exists := files[header.Name]; exists {
			return nil, fmt.Errorf("duplicate backup entry: %s", header.Name)
		}
		if _, err := importDestination(header.Name); err != nil {
			return nil, err
		}
		files[header.Name] = content
	}
	if !manifestSeen {
		return nil, errors.New("backup manifest is missing")
	}
	if metadata.FormatVersion != formatVersion {
		return nil, fmt.Errorf("unsupported backup format version: %d", metadata.FormatVersion)
	}
	actualFiles := make([]string, 0, len(files))
	for name := range files {
		actualFiles = append(actualFiles, name)
	}
	slices.Sort(actualFiles)
	expectedFiles := slices.Clone(metadata.Files)
	slices.Sort(expectedFiles)
	if !slices.Equal(actualFiles, expectedFiles) {
		return nil, errors.New("backup manifest does not match archive contents")
	}
	return files, nil
}

func importDestination(archivePath string) (string, error) {
	if archivePath == "" || path.Clean(archivePath) != archivePath || strings.Contains(archivePath, "\\") {
		return "", fmt.Errorf("invalid backup entry path: %s", archivePath)
	}
	globalConfig := config.GlobalConfig()
	switch archivePath {
	case "global-config/crux.json":
		return globalConfig, nil
	case "global-config/cruxrc":
		return filepath.Join(filepath.Dir(globalConfig), "cruxrc"), nil
	case "global-data/crux.json":
		return config.GlobalConfigData(), nil
	case "global-data/connections.json":
		return filepath.Join(config.GlobalWorkspaceDir(), "connections.json"), nil
	case "accounts/accounts.json":
		accountPath, err := accounts.Path()
		if err != nil {
			return "", fmt.Errorf("resolve account store: %w", err)
		}
		return accountPath, nil
	case "global-data/plugin-state/trust.json":
		return filepath.Join(config.GlobalWorkspaceDir(), "plugin-state", "trust.json"), nil
	case "global-data/plugin-state/provenance.json":
		return filepath.Join(config.GlobalWorkspaceDir(), "plugin-state", "provenance.json"), nil
	}
	const pluginPrefix = "global-data/plugins/"
	if strings.HasPrefix(archivePath, pluginPrefix) {
		relative := strings.TrimPrefix(archivePath, pluginPrefix)
		if relative == "" || strings.HasPrefix(relative, "../") {
			return "", fmt.Errorf("invalid plugin backup path: %s", archivePath)
		}
		return filepath.Join(config.GlobalWorkspaceDir(), "plugins", filepath.FromSlash(relative)), nil
	}
	return "", fmt.Errorf("unsupported backup entry: %s", archivePath)
}

func encrypt(plaintext, password []byte) ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate backup salt: %w", err)
	}
	key := argon2.IDKey(password, salt, 3, 64*1024, 4, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialize backup encryption: %w", err)
	}
	sealed, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize backup authentication: %w", err)
	}
	nonce := make([]byte, sealed.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate backup nonce: %w", err)
	}
	header := append(append(slices.Clone(fileMagic), byte(formatVersion)), salt...)
	result := append(header, nonce...)
	result = sealed.Seal(result, nonce, plaintext, header)
	return result, nil
}

func decrypt(encrypted, password []byte) ([]byte, error) {
	headerLength := len(fileMagic) + 1 + 16
	if len(encrypted) < headerLength || !bytes.Equal(encrypted[:len(fileMagic)], fileMagic) {
		return nil, errors.New("not a Crux backup archive")
	}
	if encrypted[len(fileMagic)] != formatVersion {
		return nil, fmt.Errorf("unsupported backup encryption version: %d", encrypted[len(fileMagic)])
	}
	header := encrypted[:headerLength]
	salt := encrypted[len(fileMagic)+1 : headerLength]
	key := argon2.IDKey(password, salt, 3, 64*1024, 4, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialize backup decryption: %w", err)
	}
	sealed, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize backup authentication: %w", err)
	}
	if len(encrypted) < headerLength+sealed.NonceSize()+sealed.Overhead() {
		return nil, errors.New("backup archive is truncated")
	}
	nonce := encrypted[headerLength : headerLength+sealed.NonceSize()]
	ciphertext := encrypted[headerLength+sealed.NonceSize():]
	plaintext, err := sealed.Open(nil, nonce, ciphertext, header)
	if err != nil {
		return nil, errors.New("invalid password or damaged backup archive")
	}
	return plaintext, nil
}

func writeNewFile(filePath string, data []byte) error {
	parent := filepath.Dir(filePath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create backup directory: %w", err)
	}
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create backup: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(filePath)
		return fmt.Errorf("write backup: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(filePath)
		return fmt.Errorf("close backup: %w", err)
	}
	return nil
}

type stagedImport struct {
	archivePath string
	destination string
	root        string
	temporary   string
	backup      string
	promoted    bool
}

func restoreFiles(files map[string][]byte) error {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)

	staged := make([]stagedImport, 0, len(names))
	cleanup := func() {
		for _, item := range staged {
			if item.temporary != "" {
				_ = os.Remove(item.temporary)
			}
		}
	}
	for _, archivePath := range names {
		destination, root, err := importTarget(archivePath)
		if err != nil {
			cleanup()
			return err
		}
		destination, root, err = prepareImportDestination(root, destination)
		if err != nil {
			cleanup()
			return fmt.Errorf("restore %s: %w", archivePath, err)
		}
		temporary, err := stageImportedFile(filepath.Dir(destination), files[archivePath])
		if err != nil {
			cleanup()
			return fmt.Errorf("restore %s: %w", archivePath, err)
		}
		staged = append(staged, stagedImport{archivePath: archivePath, destination: destination, root: root, temporary: temporary})
	}

	for index := range staged {
		item := &staged[index]
		if err := validateImportDestination(item.root, item.destination); err != nil {
			rollbackErr := rollbackImports(staged[:index])
			cleanup()
			return errors.Join(fmt.Errorf("restore %s: %w", item.archivePath, err), rollbackErr)
		}
		if info, err := os.Lstat(item.destination); err == nil {
			if !info.Mode().IsRegular() {
				rollbackErr := rollbackImports(staged[:index])
				cleanup()
				return errors.Join(fmt.Errorf("restore %s: destination is not a regular file", item.archivePath), rollbackErr)
			}
			backup, backupErr := reserveImportPath(filepath.Dir(item.destination), ".crux-import-backup-*")
			if backupErr != nil {
				rollbackErr := rollbackImports(staged[:index])
				cleanup()
				return errors.Join(fmt.Errorf("restore %s: %w", item.archivePath, backupErr), rollbackErr)
			}
			if err := renameImportFile(item.destination, backup); err != nil {
				rollbackErr := rollbackImports(staged[:index])
				cleanup()
				return errors.Join(fmt.Errorf("restore %s: preserve existing file: %w", item.archivePath, err), rollbackErr)
			}
			item.backup = backup
		} else if !errors.Is(err, os.ErrNotExist) {
			rollbackErr := rollbackImports(staged[:index])
			cleanup()
			return errors.Join(fmt.Errorf("restore %s: inspect destination: %w", item.archivePath, err), rollbackErr)
		}
		if err := renameImportFile(item.temporary, item.destination); err != nil {
			rollbackErr := rollbackImports(staged[:index+1])
			cleanup()
			return errors.Join(fmt.Errorf("restore %s: commit staged file: %w", item.archivePath, err), rollbackErr)
		}
		item.temporary = ""
		item.promoted = true
	}
	for _, item := range staged {
		if item.backup != "" {
			_ = os.Remove(item.backup)
		}
	}
	return nil
}

func importTarget(archivePath string) (string, string, error) {
	destination, err := importDestination(archivePath)
	if err != nil {
		return "", "", err
	}
	switch {
	case strings.HasPrefix(archivePath, "global-config/"):
		return destination, filepath.Dir(config.GlobalConfig()), nil
	case strings.HasPrefix(archivePath, "global-data/"):
		return destination, config.GlobalWorkspaceDir(), nil
	case archivePath == "accounts/accounts.json":
		return destination, filepath.Dir(destination), nil
	default:
		return "", "", fmt.Errorf("unsupported backup entry: %s", archivePath)
	}
}

func prepareImportDestination(root, destination string) (string, string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", "", err
	}
	canonicalRoot, err := fsext.CanonicalPath(root)
	if err != nil {
		return "", "", err
	}
	relative, err := filepath.Rel(root, destination)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", errors.New("backup destination is outside its approved root")
	}
	canonicalDestination := filepath.Join(canonicalRoot, relative)
	parent := filepath.Dir(canonicalDestination)
	parentRelative, err := filepath.Rel(canonicalRoot, parent)
	if err != nil {
		return "", "", err
	}
	current := canonicalRoot
	if parentRelative != "." {
		for _, component := range strings.Split(parentRelative, string(filepath.Separator)) {
			current = filepath.Join(current, component)
			info, statErr := os.Lstat(current)
			if errors.Is(statErr, os.ErrNotExist) {
				if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
					return "", "", err
				}
				info, statErr = os.Lstat(current)
			}
			if statErr != nil {
				return "", "", statErr
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", "", fmt.Errorf("backup destination parent is not a real directory: %s", current)
			}
		}
	}
	if err := validateImportDestination(canonicalRoot, canonicalDestination); err != nil {
		return "", "", err
	}
	return canonicalDestination, canonicalRoot, nil
}

func validateImportDestination(root, destination string) error {
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return err
	}
	if !fsext.HasPrefix(parent, root) {
		return errors.New("backup destination escaped its approved root")
	}
	if info, err := os.Lstat(destination); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("backup destination is a symlink: %s", destination)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func stageImportedFile(parent string, data []byte) (string, error) {
	temporary, err := os.CreateTemp(parent, ".crux-import-stage-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		os.Remove(temporaryPath)
		return "", err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		os.Remove(temporaryPath)
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		os.Remove(temporaryPath)
		return "", err
	}
	if err := temporary.Close(); err != nil {
		os.Remove(temporaryPath)
		return "", err
	}
	return temporaryPath, nil
}

func reserveImportPath(parent, pattern string) (string, error) {
	file, err := os.CreateTemp(parent, pattern)
	if err != nil {
		return "", err
	}
	filePath := file.Name()
	if err := file.Close(); err != nil {
		os.Remove(filePath)
		return "", err
	}
	if err := os.Remove(filePath); err != nil {
		return "", err
	}
	return filePath, nil
}

func rollbackImports(staged []stagedImport) error {
	var rollbackErr error
	for index := len(staged) - 1; index >= 0; index-- {
		item := staged[index]
		if item.promoted {
			if err := os.Remove(item.destination); err != nil && !errors.Is(err, os.ErrNotExist) {
				rollbackErr = errors.Join(rollbackErr, err)
			}
		}
		if item.backup != "" {
			if err := renameImportFile(item.backup, item.destination); err != nil {
				rollbackErr = errors.Join(rollbackErr, err)
			}
		}
	}
	return rollbackErr
}
