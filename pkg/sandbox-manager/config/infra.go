package config

type InitRuntimeOptions struct {
	EnvVars     map[string]string `json:"envVars,omitempty"`
	AccessToken string            `json:"accessToken,omitempty"`
	ReInit      bool              `json:"-"`
}

const DefaultCSIMountConcurrency = 3

type CSIMountOptions struct {
	MountOptionList    []MountConfig `json:"mountOptionList"`
	MountOptionListRaw string        `json:"mountOptionListRaw"`    // the raw json string for mount options
	Concurrency        int           `json:"concurrency,omitempty"` // max concurrent CSI mount operations, 0 or negative means unlimited, default is DefaultCSIMountConcurrency
}

type MountConfig struct {
	Driver     string `json:"driver"`
	RequestRaw string `json:"requestRaw"`
}

type InplaceUpdateOptions struct {
	Image string
}
