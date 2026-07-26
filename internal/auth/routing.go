package auth

import (
	"errors"
	"strings"
	"time"
)

var ErrInvalidBuildRouteMode = errors.New("invalid build route mode")

type BuildRouteMode string

const (
	BuildRouteAuto  BuildRouteMode = "auto"
	BuildRouteBuild BuildRouteMode = "build"
	BuildRouteXAI   BuildRouteMode = "xai"
)

type InferencePlane string

const (
	InferencePlaneBuild InferencePlane = "build"
	InferencePlaneXAI   InferencePlane = "xai"
)

type BuildRoutingInfo struct {
	RouteMode               BuildRouteMode `json:"build_route_mode"`
	SuperEntitled           bool           `json:"build_super_entitled"`
	SuperEntitledOverride   bool           `json:"build_super_entitled_override"`
	BotFlagged              bool           `json:"build_bot_flagged"`
	APIFallback             bool           `json:"build_api_fallback"` // 仅用于观测；auto 每次仍从 Build 开始。
	EffectiveInferenceRoute InferencePlane `json:"build_effective_route"`
}

type BuildRoutingUpdate struct {
	RouteMode             *BuildRouteMode
	SuperEntitledOverride *bool
	APIFallback           *bool
}

func ParseBuildRouteMode(value string) (BuildRouteMode, error) {
	mode := BuildRouteMode(strings.ToLower(strings.TrimSpace(value)))
	if mode == "" {
		mode = BuildRouteAuto
	}
	switch mode {
	case BuildRouteAuto, BuildRouteBuild, BuildRouteXAI:
		return mode, nil
	default:
		return "", ErrInvalidBuildRouteMode
	}
}

func normalizeBuildRouteMode(mode BuildRouteMode) BuildRouteMode {
	normalized, err := ParseBuildRouteMode(string(mode))
	if err != nil {
		return BuildRouteAuto
	}
	return normalized
}

func buildSuperTier(tier subscriptionTier) bool {
	switch tier.Key {
	case "supergrok", "x_premium", "x_premium_plus", "supergrok_heavy", "supergrok_lite":
		return true
	default:
		return false
	}
}

func IsPaidBuildPlan(value string) bool {
	normalized := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return -1
	}, strings.TrimSpace(value))
	switch normalized {
	case "super", "supergrok", "supergrokpro", "supergrokheavy", "supergroklite",
		"grokpro", "xpremium", "xpremiumplus", "apikey":
		return true
	default:
		return false
	}
}

func buildRoutingInfo(cred *credential, mode BuildRouteMode, override, fallback bool) BuildRoutingInfo {
	return buildRoutingInfoWithBilling(cred, nil, mode, override, fallback)
}

func buildRoutingInfoWithBilling(cred *credential, billing *BillingInfo, mode BuildRouteMode, override, fallback bool) BuildRoutingInfo {
	mode = normalizeBuildRouteMode(mode)
	info := BuildRoutingInfo{
		RouteMode: mode, SuperEntitledOverride: override, APIFallback: fallback,
		EffectiveInferenceRoute: InferencePlaneBuild,
	}
	if cred == nil {
		return info
	}
	info.BotFlagged = cred.BotFlagged
	info.SuperEntitled = override || buildSuperTier(cred.Tier) || billing != nil && (billing.Paid || IsPaidBuildPlan(billing.PlanCode) || IsPaidBuildPlan(billing.PlanName))
	if cred.AuthMode == AuthModeAPIKey {
		info.EffectiveInferenceRoute = InferencePlaneXAI
		return info
	}
	switch mode {
	case BuildRouteBuild:
		info.EffectiveInferenceRoute = InferencePlaneBuild
	case BuildRouteXAI:
		info.EffectiveInferenceRoute = InferencePlaneXAI
	default:
		if info.SuperEntitled && info.BotFlagged {
			info.EffectiveInferenceRoute = InferencePlaneXAI
		}
	}
	return info
}

func (info BuildRoutingInfo) CanProbeXAI() bool {
	return info.RouteMode == BuildRouteAuto && info.SuperEntitled &&
		info.EffectiveInferenceRoute == InferencePlaneBuild
}

func (state accountState) hasPersistentAccountState(now time.Time) bool {
	if state.Disabled || state.BuildSuperEntitled || state.BuildAPIFallback ||
		normalizeBuildRouteMode(state.BuildRouteMode) != BuildRouteAuto || len(state.ModelCooldowns) > 0 {
		return true
	}
	return now.Before(state.CooldownUntil)
}
