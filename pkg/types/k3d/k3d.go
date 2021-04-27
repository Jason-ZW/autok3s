package k3d

type Options struct {
	APIPort       string   `json:"api-port,omitempty" yaml:"api-port,omitempty"`
	AgentsMemory  string   `json:"agents-memory,omitempty" yaml:"agents-memory,omitempty"`
	Envs          []string `json:"envs,omitempty" yaml:"envs,omitempty"`
	GPUs          string   `json:"gpus,omitempty" yaml:"gpus,omitempty"`
	Image         string   `json:"image,omitempty" yaml:"image,omitempty"`
	Labels        []string `json:"labels,omitempty" yaml:"labels,omitempty"`
	NoLB          bool     `json:"no-lb,omitempty" yaml:"no-lb,omitempty"`
	NoHostIP      bool     `json:"no-hostip,omitempty" yaml:"no-hostip,omitempty"`
	NoImageVolume bool     `json:"no-image-volume,omitempty" yaml:"no-image-volume,omitempty"`
	Ports         string   `json:"ports,omitempty" yaml:"ports,omitempty"`
	ServersMemory string   `json:"servers-memory,omitempty" yaml:"servers-memory,omitempty"`
	Volumes       []string `json:"volumes,omitempty" yaml:"volumes,omitempty"`
}
