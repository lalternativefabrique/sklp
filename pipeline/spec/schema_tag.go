package spec

// TagBlock declares how the publish tag is computed. Exactly one of
// Strategy / Template must be set when the block is present on a
// Pipeline; setting both, or neither, is a compile error.
type TagBlock struct {
	// Strategy selects a built-in tag format. Recognised values:
	// "date-sha" (default shape), "sha", "date". Mutually exclusive
	// with Template.
	Strategy string `yaml:"strategy,omitempty"`

	// Template is the escape hatch: a literal string whose `{{ name }}`
	// placeholders are substituted with the corresponding git-derived
	// value. Mutually exclusive with Strategy.
	Template string `yaml:"template,omitempty"`
}
