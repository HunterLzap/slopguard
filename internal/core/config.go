package core

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const ProjectDirectory = ".slopguard"

type ProjectConfig struct {
	Languages []string `yaml:"languages" json:"languages"`
}

type InitResult struct {
	Path          string `json:"path"`
	AlreadyExists bool   `json:"already_exists"`
}

func GlobalConfigDir() (string, error) {
	if dir := os.Getenv("SLOPGUARD_CONFIG_DIR"); dir != "" {
		return filepath.Abs(dir)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "slopguard"), nil
}

func configPath(projectPath string) string {
	return filepath.Join(projectPath, ProjectDirectory, "config.yaml")
}

func LoadProjectConfig(projectPath string) (ProjectConfig, error) {
	data, err := os.ReadFile(configPath(projectPath))
	if err != nil {
		return ProjectConfig{}, err
	}
	var cfg ProjectConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return ProjectConfig{}, fmt.Errorf("parse project config: %w", err)
	}
	return cfg, nil
}

func InitProjectConfig(projectPath string, detectedLangs []string) (InitResult, error) {
	path := configPath(projectPath)
	result := InitResult{Path: path}
	if _, err := os.Lstat(path); err == nil {
		result.AlreadyExists = true
		return result, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return result, err
	}
	data, err := yaml.Marshal(ProjectConfig{Languages: detectedLangs})
	if err != nil {
		return result, err
	}
	validate := func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return yaml.Unmarshal(data, new(ProjectConfig))
	}
	err = WriteFileAtomic(path, data, 0600, true, validate)
	if errors.Is(err, fs.ErrExist) {
		result.AlreadyExists = true
		return result, nil
	}
	return result, err
}

// WriteFileAtomic stages data beside its destination and validates it before publication.
// A hard link publishes a new file atomically without replacing a concurrent writer.
func WriteFileAtomic(path string, data []byte, mode fs.FileMode, exclusive bool, validate func(string) error) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".slopguard-*")
	if err != nil {
		return err
	}
	staged := file.Name()
	defer func() {
		if removeErr := os.Remove(staged); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	writeErr := file.Chmod(mode)
	if writeErr == nil {
		_, writeErr = file.Write(data)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	if validate != nil {
		if err := validate(staged); err != nil {
			return err
		}
	}
	if exclusive {
		return os.Link(staged, path)
	}
	return os.Rename(staged, path)
}
