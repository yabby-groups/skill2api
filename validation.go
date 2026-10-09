package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func validateRequest(req generateRequest, c config) error {
	if !validID(req.RequestID) {
		return errors.New("request_id must contain only letters, digits, dot, underscore, or hyphen")
	}
	if _, err := skillNames(c, req.SkillName); err != nil {
		return err
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return errors.New("prompt is required")
	}
	if strings.TrimSpace(req.Model) != req.Model || strings.ContainsRune(req.Model, '\x00') {
		return errors.New("invalid model")
	}
	if err := validateEnvironment(req.Environment); err != nil {
		return err
	}
	if _, err := resolveOutputDir(req.OutputDir, c.OutputRoot); err != nil {
		return err
	}
	return nil
}

// skillNames parses the public comma-separated skill_name field and verifies
// that every selected package remains within the configured skills directory.
func skillNames(c config, raw string) ([]string, error) {
	if len(raw) > maxSkillName || strings.TrimSpace(raw) == "" {
		return nil, errors.New("invalid skill_name")
	}
	skillRoot, err := filepath.Abs(c.SkillsDir)
	if err != nil {
		return nil, err
	}
	resolvedRoot, err := filepath.EvalSymlinks(skillRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve skills directory: %w", err)
	}
	seen := make(map[string]struct{})
	names := make([]string, 0, strings.Count(raw, ",")+1)
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" || filepath.Base(name) != name || strings.Contains(name, "..") {
			return nil, errors.New("invalid skill_name")
		}
		if _, exists := seen[name]; exists {
			return nil, errors.New("duplicate skill_name")
		}
		skillDir := filepath.Join(skillRoot, name)
		if !within(skillRoot, skillDir) {
			return nil, errors.New("invalid skill_name")
		}
		resolvedDir, err := filepath.EvalSymlinks(skillDir)
		if err != nil || !within(resolvedRoot, resolvedDir) {
			return nil, errors.New("invalid skill_name")
		}
		info, err := os.Stat(resolvedDir)
		if err != nil {
			return nil, fmt.Errorf("skill not found: %w", err)
		}
		if !info.IsDir() {
			return nil, errors.New("skill not found: not a directory")
		}
		skillFile := filepath.Join(resolvedDir, "SKILL.md")
		resolvedFile, err := filepath.EvalSymlinks(skillFile)
		if err != nil || !within(resolvedDir, resolvedFile) {
			return nil, errors.New("invalid skill_name")
		}
		if _, err := os.Stat(resolvedFile); err != nil {
			return nil, fmt.Errorf("skill not found: %w", err)
		}
		if err := validateSkillResources(resolvedDir); err != nil {
			return nil, err
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names, nil
}

func validateSkillResources(dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == dir || info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		resolved, err := filepath.EvalSymlinks(path)
		resource, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		if err != nil {
			return fmt.Errorf("resolve skill resource symlink %q: %w", resource, err)
		}
		if !withinOrSame(dir, resolved) {
			return fmt.Errorf("skill resource symlink %q escapes skill directory", resource)
		}
		return nil
	})
}

func withinOrSame(root, path string) bool {
	return filepath.Clean(root) == filepath.Clean(path) || within(root, path)
}

func skillDir(c config, name string) (string, error) {
	names, err := skillNames(c, name)
	if err != nil || len(names) != 1 {
		if err == nil {
			err = errors.New("invalid skill_name")
		}
		return "", err
	}
	root, err := filepath.Abs(c.SkillsDir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(filepath.Join(root, names[0]))
}

func readSkills(c config, raw string) ([][]byte, []string, error) {
	names, err := skillNames(c, raw)
	if err != nil {
		return nil, nil, err
	}
	skills := make([][]byte, 0, len(names))
	for _, name := range names {
		dir, err := skillDir(c, name)
		if err != nil {
			return nil, nil, err
		}
		skill, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
		if err != nil {
			return nil, nil, fmt.Errorf("read skill %q: %w", name, err)
		}
		skills = append(skills, skill)
	}
	return skills, names, nil
}

func validateEnvironment(values map[string]string) error {
	for key, value := range values {
		if !validEnvironmentName(key) || strings.ContainsRune(value, '\x00') {
			return errors.New("invalid environment")
		}
	}
	return nil
}

func validEnvironmentName(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func resolveOutputDir(raw, root string) (string, error) {
	rel := strings.TrimSpace(raw)
	if rel == "" || filepath.IsAbs(rel) {
		return "", errors.New("output_dir must be relative to SKILL2API_OUTPUT_ROOT")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	out, err := filepath.Abs(filepath.Join(root, rel))
	if err != nil {
		return "", err
	}
	if !within(root, out) {
		return "", errors.New("output_dir must be inside SKILL2API_OUTPUT_ROOT")
	}
	return out, nil
}
func validID(s string) bool {
	if s == "" || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "."
}

func (s *statusStore) recoverRunning() error {
	entries, err := os.ReadDir(s.root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		v, err := s.read(entry.Name())
		if err != nil || v.Status != "running" {
			continue
		}
		v.Error = "worker interrupted"
		if v.SessionID != "" {
			v.Status = "interrupted"
			v.FinishedAt = ""
		} else {
			v.Status = "failed"
			v.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		log.Printf("event=skill2api_recover request_id=%s status=%s error=%q", v.RequestID, v.Status, v.Error)
		if err := s.write(v); err != nil {
			return err
		}
	}
	return nil
}
