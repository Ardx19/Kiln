// Package generator recognizes which tool configured a build directory
// (CMake or Meson) and reads what install needs from it: the project name,
// the install prefix and the command that installs into a staging root.
package generator

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Kind of build system.
type Kind string

// Build systems.
const (
	CMake Kind = "cmake"
	Meson Kind = "meson"
	Other Kind = "other"
)

// Info is what kiln learns about a build directory.
type Info struct {
	Kind    Kind
	Project string // the project's name
	Prefix  string // where it installs
	Source  string // the source directory, when known
	// Configs lists a multi-config CMake build's configurations; empty
	// otherwise.
	Configs []string
	// cmake is the CMake that configured the tree.
	cmake string
}

// Detect reads dir (a build directory).
func Detect(dir string) (Info, error) {
	if b, err := os.ReadFile(filepath.Join(dir, "CMakeCache.txt")); err == nil {
		return detectCMake(string(b)), nil
	}
	if b, err := os.ReadFile(filepath.Join(dir, "meson-info", "meson-info.json")); err == nil {
		return detectMeson(dir, b)
	}
	return Info{Kind: Other}, nil
}

func cacheValue(cache, name string) string {
	for _, line := range strings.Split(cache, "\n") {
		if rest, ok := strings.CutPrefix(line, name+":"); ok {
			if _, v, ok := strings.Cut(rest, "="); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

func detectCMake(cache string) Info {
	i := Info{Kind: CMake,
		Project: cacheValue(cache, "CMAKE_PROJECT_NAME"),
		Prefix:  cacheValue(cache, "CMAKE_INSTALL_PREFIX"),
		Source:  cacheValue(cache, "CMAKE_HOME_DIRECTORY"),
		cmake:   cacheValue(cache, "CMAKE_COMMAND"),
	}
	if strings.Contains(cacheValue(cache, "CMAKE_GENERATOR"), "Multi-Config") {
		for _, c := range strings.Split(cacheValue(cache, "CMAKE_CONFIGURATION_TYPES"), ";") {
			if c != "" {
				i.Configs = append(i.Configs, c)
			}
		}
	}
	return i
}

func detectMeson(dir string, info []byte) (Info, error) {
	var mi struct {
		Directories struct {
			Source string `json:"source"`
		} `json:"directories"`
	}
	if err := json.Unmarshal(info, &mi); err != nil {
		return Info{}, fmt.Errorf("reading meson-info.json: %v", err)
	}
	i := Info{Kind: Meson, Source: mi.Directories.Source}
	if b, err := os.ReadFile(filepath.Join(dir, "meson-info", "intro-projectinfo.json")); err == nil {
		var pi struct {
			Name string `json:"descriptive_name"`
		}
		if json.Unmarshal(b, &pi) == nil {
			i.Project = pi.Name
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "meson-info", "intro-buildoptions.json")); err == nil {
		var opts []struct {
			Name  string `json:"name"`
			Value any    `json:"value"`
		}
		if json.Unmarshal(b, &opts) == nil {
			for _, o := range opts {
				if s, ok := o.Value.(string); ok && o.Name == "prefix" {
					i.Prefix = s
				}
			}
		}
	}
	return i, nil
}

// Ctest returns the CTest that goes with the CMake that configured the
// tree, or "ctest" if that isn't known.
func (i Info) Ctest() string {
	if i.cmake != "" {
		c := filepath.Join(filepath.Dir(i.cmake), "ctest")
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "ctest"
}

// StageCommand returns the command that installs the build in dir into the
// staging root stage (it ends up under stage/<prefix>). config picks a
// configuration of a multi-config CMake build ("" for the first).
func (i Info) StageCommand(dir, config string) (*exec.Cmd, error) {
	switch i.Kind {
	case CMake:
		cmake := i.cmake
		if cmake == "" {
			cmake = "cmake"
		}
		args := []string{"--install", dir}
		if len(i.Configs) > 0 {
			if config == "" {
				config = i.Configs[0]
			}
			args = append(args, "--config", config)
		}
		cmd := exec.Command(cmake, args...)
		return cmd, nil
	case Meson:
		meson := os.Getenv("MESON")
		if meson == "" {
			meson = "meson"
		}
		return exec.Command(meson, "install", "-C", dir, "--no-rebuild"), nil
	}
	return nil, fmt.Errorf("don't know how to install a %s build; set [install] command in .kiln/config.toml (it must honour DESTDIR)", i.Kind)
}
