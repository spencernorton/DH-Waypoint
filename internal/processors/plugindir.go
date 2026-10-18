package processors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"gopkg.in/yaml.v3"
)

// Plugin files are the drag-in interface over the processor registry.
// A file named `<id>.plugin.yml` in the plugins directory (config
// `processors.plugins_dir`, env PROCESSORS_PLUGINS_DIR, default
// "plugins") declares one processor registration:
//
//	id: my-filter            # optional; defaults to the file name
//	driver: builtin/filter-list
//	mode: shadow             # shadow (default) or apply
//	priority: 100
//	configuration: {reason: adult}
//
// The processors worker reconciles the directory into the registry
// every 30 seconds: dropping a file in registers the plugin, editing
// it updates the registration, deleting it (or setting
// `enabled: false`) deregisters it — no rebuild, no restart, no API
// call. Registrations created through the operator API are never
// modified or removed by directory sync; if a file claims an ID the
// API already owns with different settings, the file is skipped and
// the conflict is logged. File ownership is tracked in process
// memory, so a file deleted while the core is stopped leaves its
// registration in place until it is removed via the API or a sync
// run that still remembers owning it.

const defaultPluginPriority = 100

var pluginIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]{0,95}$`)

// PluginFile is one parsed `<id>.plugin.yml` declaration.
type PluginFile struct {
	ID       string
	Path     string
	Enabled  bool
	write    v1.RegistryWrite
	selector *v1.Selector
}

type pluginFileYAML struct {
	ID            string         `yaml:"id"`
	Enabled       *bool          `yaml:"enabled"`
	Driver        string         `yaml:"driver"`
	Endpoint      string         `yaml:"endpoint"`
	Mode          string         `yaml:"mode"`
	State         string         `yaml:"state"`
	Priority      *int           `yaml:"priority"`
	AuthEnv       string         `yaml:"auth_env"`
	Selector      map[string]any `yaml:"selector"`
	Configuration map[string]any `yaml:"configuration"`
}

func pluginIDFromPath(path string) string {
	base := filepath.Base(path)
	for _, suffix := range []string{".plugin.yml", ".plugin.yaml"} {
		if strings.HasSuffix(base, suffix) {
			return strings.TrimSuffix(base, suffix)
		}
	}
	return base
}

// ParsePluginFile reads and validates one plugin declaration. Unknown
// keys are an error: a misspelled setting must fail loudly here rather
// than be silently ignored at runtime.
func ParsePluginFile(path string) (PluginFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PluginFile{}, err
	}
	if len(data) > 64*1024 {
		return PluginFile{}, fmt.Errorf("%s: plugin file exceeds 64 KiB", path)
	}
	return parsePluginFile(path, data)
}

func parsePluginFile(path string, data []byte) (PluginFile, error) {
	var raw pluginFileYAML
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return PluginFile{}, fmt.Errorf("%s: %w", path, err)
	}

	id := raw.ID
	if id == "" {
		id = pluginIDFromPath(path)
	}
	if !pluginIDPattern.MatchString(id) {
		return PluginFile{}, fmt.Errorf("%s: invalid plugin id %q", path, id)
	}

	mode := raw.Mode
	if mode == "" {
		mode = "shadow"
	}
	if mode != "shadow" && mode != "apply" {
		return PluginFile{}, fmt.Errorf("%s: mode must be shadow or apply, got %q", path, raw.Mode)
	}
	state := raw.State
	if state == "" {
		state = "active"
	}
	priority := defaultPluginPriority
	if raw.Priority != nil {
		priority = *raw.Priority
	}
	enabled := true
	if raw.Enabled != nil {
		enabled = *raw.Enabled
	}

	config := raw.Configuration
	if config == nil {
		config = map[string]any{}
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		return PluginFile{}, fmt.Errorf("%s: configuration: %w", path, err)
	}

	var selector *v1.Selector
	if raw.Selector != nil {
		selJSON, err := json.Marshal(raw.Selector)
		if err != nil {
			return PluginFile{}, fmt.Errorf("%s: selector: %w", path, err)
		}
		selector = &v1.Selector{}
		if err := v1.Decode(selJSON, selector); err != nil {
			return PluginFile{}, fmt.Errorf("%s: selector: %w", path, err)
		}
	}

	spec := PluginFile{
		ID:      id,
		Path:    path,
		Enabled: enabled,
		write: v1.RegistryWrite{
			Driver:        raw.Driver,
			Endpoint:      raw.Endpoint,
			Mode:          mode,
			State:         state,
			Priority:      priority,
			Selector:      selector,
			Configuration: configJSON,
			AuthEnv:       raw.AuthEnv,
		},
		selector: selector,
	}
	if err := validateWrite(spec.write); err != nil {
		return PluginFile{}, fmt.Errorf("%s: %w", path, err)
	}
	return spec, nil
}

// Write returns the registry write this file declares.
func (f PluginFile) Write() v1.RegistryWrite { return f.write }

func normalizedDriver(driver string) string {
	if driver == "" {
		return v1.DriverHTTP
	}
	return driver
}

// Matches reports whether the registry entry already carries exactly
// the deployment settings this file declares.
func (f PluginFile) Matches(e Entry) bool {
	if e.Source != "file" {
		return false
	}
	if e.ID != f.ID ||
		normalizedDriver(e.Driver) != normalizedDriver(f.write.Driver) ||
		e.Endpoint != f.write.Endpoint ||
		e.AuthEnv != f.write.AuthEnv ||
		e.Mode != f.write.Mode ||
		e.State != f.write.State ||
		e.Priority != f.write.Priority {
		return false
	}
	if !bytes.Equal(canonical(e.Configuration), canonical(f.write.Configuration)) {
		return false
	}
	if e.Learning != nil && e.Learning.Enabled {
		return false
	}
	entrySel, _ := json.Marshal(e.Selector)
	fileSel, _ := json.Marshal(f.selector)
	entryEmpty := e.Selector.IsEmpty()
	fileEmpty := f.selector.IsEmpty()
	if entryEmpty && fileEmpty {
		return true
	}
	return bytes.Equal(entrySel, fileSel)
}

// LoadPluginDir parses every `*.plugin.yml`/`*.plugin.yaml` in dir. A
// missing directory is an empty result, not an error. Files that fail
// to parse are skipped and reported in the returned error; valid
// files are still returned.
func LoadPluginDir(dir string) ([]PluginFile, error) {
	specs, _, err := loadPluginDir(dir)
	return specs, err
}

func loadPluginDir(dir string) ([]PluginFile, map[string]bool, error) {
	present := map[string]bool{}
	if dir == "" {
		return nil, present, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, present, nil
		}
		return nil, present, err
	}
	names := []string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".plugin.yml") || strings.HasSuffix(name, ".plugin.yaml") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) > 64 {
		names = names[:64]
	}

	var specs []PluginFile
	var errs []error
	seen := map[string]string{}
	for _, name := range names {
		path := filepath.Join(dir, name)
		// The would-be ID counts as present even when the file fails
		// to parse, so a typo mid-edit never deregisters a plugin.
		present[pluginIDFromPath(path)] = true
		spec, err := ParsePluginFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		present[spec.ID] = true
		if other, dup := seen[spec.ID]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate plugin id %q also declared in %s", path, spec.ID, other))
			continue
		}
		seen[spec.ID] = path
		specs = append(specs, spec)
	}
	return specs, present, errors.Join(errs...)
}

// SyncPluginDir reconciles the plugins directory into the registry.
// It is safe to call repeatedly; unchanged files cause no registry
// writes.
func (s *Store) SyncPluginDir(ctx context.Context) error {
	dir := s.cfg.PluginsDir
	if dir == "" {
		return nil
	}
	specs, present, loadErr := loadPluginDir(dir)

	s.pluginMu.Lock()
	if s.pluginOwned == nil {
		s.pluginOwned = map[string]bool{}
	}
	s.pluginMu.Unlock()

	current, err := s.Entries(ctx)
	if err != nil {
		return err
	}
	byID := make(map[string]Entry, len(current))
	for _, e := range current {
		byID[e.ID] = e
	}

	var errs []error
	if loadErr != nil {
		errs = append(errs, loadErr)
	}

	desired := map[string]bool{}
	for _, spec := range specs {
		if !spec.Enabled {
			continue
		}
		desired[spec.ID] = true
		entry, exists := byID[spec.ID]
		if exists && spec.Matches(entry) {
			s.markPluginOwned(spec.ID)
			continue
		}
		if exists && entry.Source != "file" && !s.isPluginOwned(spec.ID) {
			s.log().Warnw("plugin file conflicts with API-managed registration; file skipped",
				"plugin", spec.ID, "file", spec.Path)
			continue
		}
		write := spec.Write()
		if exists {
			write.ExpectedGeneration = entry.Generation
		}
		if _, err := s.RegisterSource(ctx, spec.ID, write, "file"); err != nil {
			errs = append(errs, fmt.Errorf("plugin %s (%s): %w", spec.ID, spec.Path, err))
			continue
		}
		s.markPluginOwned(spec.ID)
	}

	// Deregister file-managed plugins whose file was removed or
	// disabled. IDs still present on disk (even unparsable) are kept.
	// Ownership unions the persisted registry source (survives
	// restarts) with this process's marks.
	s.pluginMu.Lock()
	ownedSet := make(map[string]bool, len(s.pluginOwned))
	for id := range s.pluginOwned {
		ownedSet[id] = true
	}
	s.pluginMu.Unlock()
	for _, e := range current {
		if e.Source == "file" {
			ownedSet[e.ID] = true
		}
	}
	owned := make([]string, 0, len(ownedSet))
	for id := range ownedSet {
		owned = append(owned, id)
	}
	for _, id := range owned {
		if desired[id] || present[id] {
			continue
		}
		entry, exists := byID[id]
		if !exists {
			s.unmarkPluginOwned(id)
			continue
		}
		if err := s.Remove(ctx, id, entry.Generation); err != nil {
			errs = append(errs, fmt.Errorf("plugin %s deregister: %w", id, err))
			continue
		}
		s.unmarkPluginOwned(id)
	}
	return errors.Join(errs...)
}

func (s *Store) markPluginOwned(id string) {
	s.pluginMu.Lock()
	defer s.pluginMu.Unlock()
	if s.pluginOwned == nil {
		s.pluginOwned = map[string]bool{}
	}
	s.pluginOwned[id] = true
}

func (s *Store) unmarkPluginOwned(id string) {
	s.pluginMu.Lock()
	defer s.pluginMu.Unlock()
	delete(s.pluginOwned, id)
}

func (s *Store) isPluginOwned(id string) bool {
	s.pluginMu.Lock()
	defer s.pluginMu.Unlock()
	return s.pluginOwned[id]
}
