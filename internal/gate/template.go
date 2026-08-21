package gate

import (
	"bytes"
	"fmt"
	"text/template"
)

// RenderCommand expands each command argument independently using Settings.
func (b Binding) RenderCommand() ([]string, error) {
	rendered := make([]string, len(b.Command))
	for index, argument := range b.Command {
		tmpl, err := template.New("argument").Option("missingkey=error").Parse(argument)
		if err != nil {
			return nil, fmt.Errorf("command argument %d: parse template: %w", index, err)
		}

		var output bytes.Buffer
		if err := tmpl.Execute(&output, b.Settings); err != nil {
			return nil, fmt.Errorf("command argument %d: render template: %w", index, err)
		}
		rendered[index] = output.String()
	}
	return rendered, nil
}
