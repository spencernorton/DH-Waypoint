package classifier

import (
	_ "embed"
)

//go:embed classifier.core.yml
var classifierCoreYaml []byte

// CoreSource returns the built-in classifier definition on its own, without
// anything an operator's classifier file or configuration adds to it.
func CoreSource() (Source, error) {
	return yamlSourceProvider{rawSourceProvider: coreSourceProvider{}}.source()
}
