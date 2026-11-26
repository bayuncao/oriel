package store

import (
	"context"

	"github.com/bayuncao/oriel/internal/agent"
)

// 設定表裡 Agent 目錄的兩筆（m4.md 第 2 節）：使用者的設定，與上一次的發現結果。
const (
	agentConfigKey   = "agents"
	installationsKey = "agents.installations"
)

// AgentConfig 回傳加入的 Registry 條目、自訂 agent 與覆蓋路徑；沒存過時是零值（agent.ConfigStore）。
func (s *Store) AgentConfig(ctx context.Context) (agent.Config, error) {
	var c agent.Config
	_, err := s.getSetting(ctx, agentConfigKey, &c)
	return c, err
}

// PutAgentConfig 換掉 Agent 目錄的設定（agent.ConfigStore）。
func (s *Store) PutAgentConfig(ctx context.Context, c agent.Config) error {
	return s.putSetting(ctx, agentConfigKey, c)
}

// LastInstallations 回傳上一次存下的發現結果；沒存過時是 nil（agent.ConfigStore）。
func (s *Store) LastInstallations(ctx context.Context) ([]agent.Installation, error) {
	var t []agent.Installation
	_, err := s.getSetting(ctx, installationsKey, &t)
	return t, err
}

// PutLastInstallations 存下這一次的發現結果（agent.ConfigStore）。
func (s *Store) PutLastInstallations(ctx context.Context, t []agent.Installation) error {
	return s.putSetting(ctx, installationsKey, t)
}
