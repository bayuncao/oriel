//go:build !darwin

package workspace

import (
	"context"

	"github.com/bayuncao/oriel/internal/proc"
)

// usable 在非 darwin 上不需要 CLT 檢查。
func (g *Git) usable(context.Context, proc.Env, string) bool { return true }
