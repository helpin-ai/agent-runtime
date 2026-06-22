package skills

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type archiveFS map[string][]byte

type archiveFile struct {
	*bytes.Reader
	name string
	size int64
}

type archiveFileInfo struct {
	name string
	size int64
}

func (f archiveFS) Open(name string) (fs.File, error) {
	clean := path.Clean(strings.TrimSpace(name))
	if clean == "." {
		clean = ""
	}
	payload, ok := f[clean]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return &archiveFile{Reader: bytes.NewReader(payload), name: path.Base(clean), size: int64(len(payload))}, nil
}

func (f *archiveFile) Stat() (fs.FileInfo, error) {
	return archiveFileInfo{name: f.name, size: f.size}, nil
}

func (f *archiveFile) Close() error {
	return nil
}

func (fi archiveFileInfo) Name() string       { return fi.name }
func (fi archiveFileInfo) Size() int64        { return fi.size }
func (fi archiveFileInfo) Mode() fs.FileMode  { return 0o444 }
func (fi archiveFileInfo) ModTime() time.Time { return time.Time{} }
func (fi archiveFileInfo) IsDir() bool        { return false }
func (fi archiveFileInfo) Sys() any           { return nil }

func LoadSkillArchive(data []byte, sourceKind string) (Definition, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Definition{}, fmt.Errorf("read skill archive: %w", err)
	}
	files := make(archiveFS)
	roots := make(map[string]struct{})
	for _, file := range zr.File {
		cleanPath, err := normalizeArchivePath(file.Name)
		if err != nil {
			return Definition{}, err
		}
		if cleanPath == "" || strings.HasSuffix(cleanPath, "/") {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return Definition{}, fmt.Errorf("open %q: %w", file.Name, err)
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return Definition{}, fmt.Errorf("read %q: %w", file.Name, err)
		}
		files[cleanPath] = content
		if path.Base(cleanPath) == "SKILL.md" {
			roots[path.Dir(cleanPath)] = struct{}{}
		}
	}
	if len(roots) == 0 {
		return Definition{}, fmt.Errorf("archive does not contain SKILL.md")
	}
	if len(roots) > 1 {
		return Definition{}, fmt.Errorf("archive contains multiple skill roots")
	}
	var root string
	for candidate := range roots {
		root = candidate
	}
	if root == "." {
		root = ""
	}
	definition, err := LoadPackage(files, root, sourceKind)
	if err != nil {
		return Definition{}, err
	}
	definition.SourceKind = strings.TrimSpace(sourceKind)
	return definition, nil
}

func BuildSkillArchive(def Definition) ([]byte, string, string, error) {
	key := strings.TrimSpace(def.Key)
	if key == "" {
		return nil, "", "", fmt.Errorf("skill key is required")
	}
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	root := key
	skillMarkdown, err := renderSkillMarkdown(def)
	if err != nil {
		return nil, "", "", err
	}
	if err := writeZipFile(zw, path.Join(root, "SKILL.md"), skillMarkdown); err != nil {
		return nil, "", "", err
	}
	if cfg, ok, err := renderOpenAIConfig(def); err != nil {
		return nil, "", "", err
	} else if ok {
		if err := writeZipFile(zw, path.Join(root, "agents", "openai.yaml"), cfg); err != nil {
			return nil, "", "", err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, "", "", fmt.Errorf("close skill archive: %w", err)
	}
	sum := sha256.Sum256(archive.Bytes())
	return archive.Bytes(), hex.EncodeToString(sum[:])[:12], key + ".zip", nil
}

func ExtractSkillArchiveToDir(data []byte, destDir string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("read skill archive: %w", err)
	}
	if strings.TrimSpace(destDir) == "" {
		return fmt.Errorf("destination directory is required")
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create destination directory: %w", err)
	}
	for _, file := range zr.File {
		cleanPath, err := normalizeArchivePath(file.Name)
		if err != nil {
			return err
		}
		if cleanPath == "" {
			continue
		}
		relativePath := stripArchiveRoot(cleanPath)
		if relativePath == "" {
			continue
		}
		targetPath := filepath.Join(destDir, filepath.FromSlash(relativePath))
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(targetPath, 0o755); err != nil {
				return fmt.Errorf("create archive directory %q: %w", relativePath, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return fmt.Errorf("create archive parent directory %q: %w", relativePath, err)
		}
		rc, err := file.Open()
		if err != nil {
			return fmt.Errorf("open %q: %w", file.Name, err)
		}
		payload, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("read %q: %w", file.Name, err)
		}
		if err := os.WriteFile(targetPath, payload, 0o644); err != nil {
			return fmt.Errorf("write extracted file %q: %w", relativePath, err)
		}
	}
	return nil
}

func SkillVersionForBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}

func writeZipFile(zw *zip.Writer, name, body string) error {
	writer, err := zw.Create(name)
	if err != nil {
		return fmt.Errorf("create %q in archive: %w", name, err)
	}
	if _, err := writer.Write([]byte(body)); err != nil {
		return fmt.Errorf("write %q in archive: %w", name, err)
	}
	return nil
}

func renderSkillMarkdown(def Definition) (string, error) {
	frontmatter := map[string]any{
		"name":        strings.TrimSpace(def.Key),
		"description": strings.TrimSpace(def.Description),
		"metadata": map[string]any{
			"title":              strings.TrimSpace(def.Title),
			"required_tools":     append([]string(nil), def.RequiredTools...),
			"supported_runtimes": append([]string(nil), def.SupportedRuntimes...),
		},
	}
	payload, err := yaml.Marshal(frontmatter)
	if err != nil {
		return "", fmt.Errorf("marshal skill frontmatter: %w", err)
	}
	return fmt.Sprintf("---\n%s---\n\n%s\n", string(payload), strings.TrimSpace(def.Instructions)), nil
}

func renderOpenAIConfig(def Definition) (string, bool, error) {
	cfg := packageConfig{
		Interface: def.Interface,
		Policy:    def.Policy,
	}
	if cfg.Interface == (Interface{}) && policyIsZero(cfg.Policy) {
		return "", false, nil
	}
	payload, err := yaml.Marshal(cfg)
	if err != nil {
		return "", false, fmt.Errorf("marshal skill openai config: %w", err)
	}
	return string(payload), true, nil
}

func policyIsZero(policy Policy) bool {
	return policy.AllowImplicitInvocation == nil &&
		len(policy.CompletionRequiresInteractionKinds) == 0 &&
		len(policy.InteractionContracts) == 0
}

func normalizeArchivePath(name string) (string, error) {
	clean := path.Clean(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	if clean == "." {
		return "", nil
	}
	if strings.HasPrefix(clean, "../") || clean == ".." || path.IsAbs(clean) {
		return "", fmt.Errorf("invalid archive path %q", name)
	}
	return clean, nil
}

func stripArchiveRoot(clean string) string {
	clean = path.Clean(strings.TrimSpace(clean))
	if clean == "." || clean == "" {
		return ""
	}
	if slash := strings.IndexByte(clean, '/'); slash >= 0 {
		return clean[slash+1:]
	}
	return ""
}
