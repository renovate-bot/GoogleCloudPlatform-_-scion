package api

type AgentConfig struct {
	Grove  string `json:"grove"`
	Name   string `json:"name"`
	Status string `json:"status,omitempty"`
}

type VolumeMount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

type ScionConfig struct {
	Template        string            `json:"template"`
	HarnessProvider string            `json:"harness_provider,omitempty"`
	ConfigDir       string            `json:"config_dir,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Volumes         []VolumeMount     `json:"volumes,omitempty"`
	UnixUsername    string            `json:"unix_username"`
	Image           string            `json:"image"`
	Detached        *bool             `json:"detached"`
	UseTmux         *bool             `json:"use_tmux"`
	Model           string            `json:"model"`
	Agent           *AgentConfig      `json:"agent,omitempty"`
}

func (c *ScionConfig) IsDetached() bool {
	if c.Detached == nil {
		return true
	}
	return *c.Detached
}

func (c *ScionConfig) IsUseTmux() bool {
	if c.UseTmux == nil {
		return false
	}
	return *c.UseTmux
}

type AuthConfig struct {
	GeminiAPIKey         string
	GoogleAPIKey         string
	VertexAPIKey         string
	GoogleAppCredentials string
	GoogleCloudProject   string
	OAuthCreds           string
	AnthropicAPIKey      string
}
