package agent

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bayuncao/oriel/internal/task"
)

// CatalogError 是無法加入目錄的已設定 Agent。
type CatalogError struct {
	Agent   task.AgentID `json:"agent"`
	Problem Problem      `json:"problem"`
}

// Catalog 將三個來源合併為一份清單：先是 app 註冊順序的
// builtin，然後是已加入的 Registry 條目，最後是自訂 agent。
// ID 衝突時較前面的來源獲勝（builtin > registry > custom），
// 落敗者會被回報。
func Catalog(builtins []Definition, registry []RegistryAgent, custom []CustomAgent) ([]Definition, []CatalogError) {
	var defs []Definition
	var problems []CatalogError
	seen := map[task.AgentID]bool{}
	add := func(d Definition, err error, id task.AgentID) {
		switch {
		case err != nil:
			problems = append(problems, CatalogError{Agent: id, Problem: asProblem(err)})
		case seen[d.ID]:
			problems = append(problems, CatalogError{Agent: d.ID, Problem: problem(CodeDuplicateAgent)})
		default:
			seen[d.ID] = true
			defs = append(defs, d)
		}
	}
	for _, b := range builtins {
		b.Source = SourceBuiltin
		add(b, nil, b.ID)
	}
	for _, r := range registry {
		d, err := r.Definition()
		add(d, err, registryID(r.ID))
	}
	for _, c := range custom {
		d, err := c.Definition()
		add(d, err, customID(c.Slug))
	}
	return defs, problems
}

// registryID 與 customID 是 Registry 條目與自訂 agent 的
// Agent ID；builtin 的 ID 沒有前綴。
func registryID(id string) task.AgentID { return task.AgentID("registry:" + id) }
func customID(slug string) task.AgentID { return task.AgentID("custom:" + slug) }

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Definition 是目錄執行自訂 agent 時的形式：ACP、無身分
// 檢查、版本未知。
func (c CustomAgent) Definition() (Definition, error) {
	switch {
	case !slugRE.MatchString(c.Slug):
		return Definition{}, fail(CodeInvalidSlug, nil, "slug", c.Slug)
	case c.Command == "":
		return Definition{}, fail(CodeNoCommand, nil, "slug", c.Slug)
	case strings.ContainsRune(c.Command, filepath.Separator) && !filepath.IsAbs(c.Command):
		return Definition{}, fail(CodeRelativeCommand, nil, "slug", c.Slug, "command", c.Command)
	}
	name := c.Name
	if name == "" {
		name = c.Slug
	}
	return Definition{
		ID:       customID(c.Slug),
		Name:     name,
		Source:   SourceCustom,
		Protocol: task.ProtocolACP,
		Detect:   DetectSpec{Command: c.Command},
		Launch:   LaunchSpec{Args: c.Args, Env: c.Env},
	}, nil
}
