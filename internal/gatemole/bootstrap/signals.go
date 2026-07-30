package bootstrap

const (
	Version = "gatemole.bootstrap_report.v0"
)

type Options struct {
	DryRun     bool
	Check      bool
	Aggressive bool
}

type Result struct {
	Version    string   `json:"version"`
	Repo       string   `json:"repo"`
	Mode       string   `json:"mode"`
	Drafts     []Draft  `json:"drafts"`
	Wrote      []string `json:"wrote"`
	ReportPath string   `json:"report_path,omitempty"`
	NeedsWrite bool     `json:"needs_write"`
	Check      bool     `json:"check,omitempty"`
}

type Draft struct {
	Component   string       `json:"component"`
	Owner       string       `json:"owner"`
	Risk        string       `json:"risk"`
	Paths       []string     `json:"paths"`
	Signals     []Signal     `json:"signals"`
	Obligations []Obligation `json:"obligations"`
	IntentPath  string       `json:"intent_path"`
}

type Signal struct {
	Type   string `json:"type"`
	File   string `json:"file,omitempty"`
	Symbol string `json:"symbol,omitempty"`
	Detail string `json:"detail,omitempty"`
	Risk   string `json:"risk,omitempty"`
}

type Obligation struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Description string    `json:"description"`
	Generated   Generated `json:"generated"`
}

type Generated struct {
	By         string       `json:"by"`
	Mode       string       `json:"mode"`
	Confidence string       `json:"confidence"`
	Source     SignalSource `json:"source"`
}

type SignalSource struct {
	Type   string `json:"type"`
	File   string `json:"file,omitempty"`
	Symbol string `json:"symbol,omitempty"`
	Detail string `json:"detail,omitempty"`
}
