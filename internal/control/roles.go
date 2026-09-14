package control

import "fmt"

type Role string

const (
	RoleController Role = "controller"
	RoleManager    Role = "manager"
	RoleWorker     Role = "worker"
)

type Capabilities struct {
	Protocol          int    `json:"protocol"`
	PromotionShortcut string `json:"promotionShortcut"`
}

type Requester struct {
	PaneID string `json:"paneId"`
	Role   Role   `json:"role"`
	RootID string `json:"rootId"`
	Human  bool   `json:"human"`
}

func ParseRole(value string) (Role, error) {
	role := Role(value)
	switch role {
	case RoleController, RoleManager, RoleWorker:
		return role, nil
	default:
		return "", fmt.Errorf("invalid agency role %q", value)
	}
}

func (r Requester) ChildRole(requested Role) (Role, error) {
	allowed := r.Role == RoleController && requested == RoleManager ||
		r.Role == RoleManager && requested == RoleWorker
	if !allowed {
		if requested == "" {
			return "", fmt.Errorf("%s panes must request an explicit child role", r.Role)
		}
		return "", fmt.Errorf("%s panes cannot create %s panes", r.Role, requested)
	}
	return requested, nil
}

func (r Requester) CanKillPane(paneID string) bool {
	return r.Role != RoleWorker || r.PaneID == paneID
}

func (r Requester) CanMovePane(paneID string) bool {
	return r.Role != RoleWorker || r.PaneID == paneID
}

func (r Requester) CanKillWindow() bool {
	return r.Role != RoleWorker
}
