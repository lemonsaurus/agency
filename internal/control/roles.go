package control

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Role string

const (
	RoleController Role = "controller"
	RoleManager    Role = "manager"
	RoleWorker     Role = "worker"
)

type Capabilities struct {
	Protocol          int    `json:"protocol"`
	PaneLabels        bool   `json:"paneLabels"`
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

func (r Requester) CanLabelPane(paneID string) bool {
	return r.Human || r.Role == RoleController || r.Role == RoleManager ||
		r.Role == RoleWorker && r.PaneID == paneID
}

func ValidateTaskLabel(label string, required bool) error {
	if required && strings.TrimSpace(label) == "" {
		return fmt.Errorf("programmatic spawns require a nonblank task label")
	}
	if !utf8.ValidString(label) || utf8.RuneCountInString(label) > 100 {
		return fmt.Errorf("task label must be valid Unicode and at most 100 characters")
	}
	for _, r := range label {
		if unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp) {
			return fmt.Errorf("task label must be a single line without control characters")
		}
	}
	return nil
}

func (r Requester) CanKillWindow() bool {
	return r.Role != RoleWorker
}
