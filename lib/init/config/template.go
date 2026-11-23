package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"text/template"
)

// TemplateGuest is the value of File.Template that renders the file from the booted image's guest.json.
const TemplateGuest = "guest"

// guestFuncs is what a guest template calls: json writes a value as JSON, so a kernel map or a
// digest lands in the file as the JSON it is and no quoting is the template's.
var guestFuncs = template.FuncMap{
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
}

func (f File) parseTemplate() (*template.Template, error) {
	return template.New(f.Path).Option("missingkey=error").Funcs(guestFuncs).Parse(f.Content)
}

// checkTemplate reports why the file's template is not one: a name other than "guest" or content that
// does not parse. A file with no template has nothing to check.
func (f File) checkTemplate() error {
	switch f.Template {
	case "":
		return nil
	case TemplateGuest:
		if _, err := f.parseTemplate(); err != nil {
			return fmt.Errorf("content is not a template: %w", err)
		}
		return nil
	}
	return fmt.Errorf("must be %q or empty, got %q", TemplateGuest, f.Template)
}

// Render returns the bytes init writes for the file: Content as it stands, or, for a guest template,
// Content executed over guest, the bytes of the booted image's guest.json. guest.json is the one place
// the image names its guest files and their digests, so a configuration that carries them as a
// template carries no second copy to drift when an upgrade changes them. A key the template names
// and guest.json lacks is an error and writes nothing.
func (f File) Render(guest func() ([]byte, error)) ([]byte, error) {
	if f.Template == "" {
		return []byte(f.Content), nil
	}
	if err := f.checkTemplate(); err != nil {
		return nil, fmt.Errorf("%s: %w", f.Path, err)
	}
	raw, err := guest()
	if err != nil {
		return nil, fmt.Errorf("%s: read the guest manifest: %w", f.Path, err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("%s: the guest manifest is not JSON: %w", f.Path, err)
	}
	t, _ := f.parseTemplate()
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return nil, fmt.Errorf("%s: render from the guest manifest: %w", f.Path, err)
	}
	return b.Bytes(), nil
}
