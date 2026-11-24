package operatorui

import _ "embed"

//go:embed index.html
var indexHTML []byte

// IndexHTML returns the embedded Operator Exception Dashboard single-page application.
func IndexHTML() []byte {
	return indexHTML
}
