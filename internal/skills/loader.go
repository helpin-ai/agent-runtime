package skills

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

type packageConfig struct {
	Interface    Interface `yaml:"interface"`
	Policy       Policy    `yaml:"policy"`
	Dependencies struct {
		Tools []SkillDependency `yaml:"tools,omitempty"`
	} `yaml:"dependencies"`
}

type SkillDependency struct {
	Type        string `json:"type,omitempty" yaml:"type"`
	Value       string `json:"value,omitempty" yaml:"value"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Transport   string `json:"transport,omitempty" yaml:"transport,omitempty"`
	URL         string `json:"url,omitempty" yaml:"url,omitempty"`
}

type frontmatter struct {
	Name        string             `yaml:"name"`
	Description string             `yaml:"description"`
	Metadata    frontmatterDetails `yaml:"metadata"`
}

type frontmatterDetails struct {
	Title             string   `yaml:"title"`
	RequiredTools     []string `yaml:"required_tools"`
	SupportedRuntimes []string `yaml:"supported_runtimes"`
}

func LoadBuiltInSkills(skillFS fs.FS, root string) ([]Definition, error) {
	entries, err := fs.ReadDir(skillFS, root)
	if err != nil {
		return nil, fmt.Errorf("read built-in skill root %q: %w", root, err)
	}
	out := make([]Definition, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skill, err := LoadPackage(skillFS, path.Join(root, entry.Name()), SourceBuiltIn)
		if err != nil {
			return nil, err
		}
		out = append(out, skill)
	}
	return out, nil
}

func LoadPackage(skillFS fs.FS, packagePath string, sourceKind string) (Definition, error) {
	skillDocPath := path.Join(packagePath, "SKILL.md")
	payload, err := fs.ReadFile(skillFS, skillDocPath)
	if err != nil {
		return Definition{}, fmt.Errorf("read %q: %w", skillDocPath, err)
	}
	frontmatter, body, err := parseMarkdown(payload)
	if err != nil {
		return Definition{}, fmt.Errorf("parse %q: %w", skillDocPath, err)
	}
	config, err := loadConfig(skillFS, path.Join(packagePath, "agents", "openai.yaml"))
	if err != nil {
		return Definition{}, err
	}
	key := strings.TrimSpace(frontmatter.Name)
	if key == "" {
		key = path.Base(packagePath)
	}
	definition := Definition{
		Key:               key,
		Title:             deriveTitle(frontmatter, config, key),
		Description:       deriveDescription(frontmatter, config),
		SourceKind:        strings.TrimSpace(sourceKind),
		PackagePath:       packagePath,
		Instructions:      strings.TrimSpace(body),
		RequiredTools:     append([]string(nil), frontmatter.Metadata.RequiredTools...),
		SupportedRuntimes: append([]string(nil), frontmatter.Metadata.SupportedRuntimes...),
		Policy:            config.Policy,
		Interface:         config.Interface,
	}
	if definition.SourceKind == "" {
		definition.SourceKind = SourceBuiltIn
	}
	definition = normalizeDefinition(definition)
	if definition.Description == "" {
		return Definition{}, fmt.Errorf("skill %q is missing a description", packagePath)
	}
	if definition.Instructions == "" {
		return Definition{}, fmt.Errorf("skill %q has empty instructions", packagePath)
	}
	return definition, nil
}

func loadConfig(skillFS fs.FS, configPath string) (packageConfig, error) {
	payload, err := fs.ReadFile(skillFS, configPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return packageConfig{}, nil
		}
		return packageConfig{}, fmt.Errorf("read %q: %w", configPath, err)
	}
	var config packageConfig
	if err := yaml.Unmarshal(payload, &config); err != nil {
		return packageConfig{}, fmt.Errorf("parse %q: %w", configPath, err)
	}
	return config, nil
}

func parseMarkdown(payload []byte) (frontmatter, string, error) {
	content := strings.ReplaceAll(string(payload), "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return frontmatter{}, "", fmt.Errorf("missing YAML frontmatter")
	}
	bodyStart := strings.Index(content[len("---\n"):], "\n---\n")
	if bodyStart < 0 {
		return frontmatter{}, "", fmt.Errorf("unterminated YAML frontmatter")
	}
	bodyStart += len("---\n")
	fmBlock := content[len("---\n"):bodyStart]
	body := strings.TrimSpace(content[bodyStart+len("\n---\n"):])

	var fm frontmatter
	if err := yaml.Unmarshal([]byte(fmBlock), &fm); err != nil {
		return frontmatter{}, "", err
	}
	fm.Name = strings.TrimSpace(fm.Name)
	fm.Description = strings.TrimSpace(fm.Description)
	fm.Metadata.Title = strings.TrimSpace(fm.Metadata.Title)
	if fm.Name == "" {
		return frontmatter{}, "", fmt.Errorf("frontmatter field \"name\" is required")
	}
	if fm.Description == "" {
		return frontmatter{}, "", fmt.Errorf("frontmatter field \"description\" is required")
	}
	return fm, body, nil
}

func deriveTitle(fm frontmatter, config packageConfig, key string) string {
	if title := strings.TrimSpace(config.Interface.DisplayName); title != "" {
		return title
	}
	if title := strings.TrimSpace(fm.Metadata.Title); title != "" {
		return title
	}
	return strings.TrimSpace(strings.ReplaceAll(key, "_", " "))
}

func deriveDescription(fm frontmatter, config packageConfig) string {
	if description := strings.TrimSpace(config.Interface.ShortDescription); description != "" {
		return description
	}
	return strings.TrimSpace(fm.Description)
}
