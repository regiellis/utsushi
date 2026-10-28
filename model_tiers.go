package main

import (
	"fmt"
	"strings"
)

// Budgets are conservative planning estimates for the shipped workflow variants,
// not measured peaks or minimum requirements. Reserve includes references and
// working memory. Offload, quantization, duration and resolution change actual use.
type memoryBudget struct{ Base, Reserve int }

var localBudgets = map[string]memoryBudget{
	"zimage": {14, 2}, "krea2": {14, 2},
	"qwen21": {24, 8}, "qwen21-turbo": {24, 8}, "qwen21-edit": {24, 8},
	"wan-animate2": {20, 4}, "minimax-h3-local": {24, 8},
	"minimax-h3-long": {24, 8}, "ltx23-iclora": {24, 8},
}

type hardwareInfo struct {
	Name, DeviceType string
	Total            int64
	Free             *int64
}

func (h hardwareInfo) summary() string {
	if h.Total <= 0 {
		return "ComfyUI GPU memory unknown · connect a server to check hardware"
	}
	s := fmt.Sprintf("ComfyUI device: %s · %.1f GiB total", h.Name, float64(h.Total)/(1<<30))
	if h.Free != nil {
		s += fmt.Sprintf(" · %.1f GiB free now", float64(*h.Free)/(1<<30))
	}
	return s
}

type modelAdvice struct {
	Tier, Detail, Warning string
	Dim                   bool
}

func memoryTier(gib int) string {
	switch {
	case gib <= 12:
		return "Low · 12 GB"
	case gib <= 20:
		return "Mid · 16–20 GB"
	case gib <= 32:
		return "High · 24–32 GB"
	default:
		return "Workstation · over 32 GB"
	}
}

func localAdvice(id string, hw hardwareInfo) modelAdvice {
	b, ok := localBudgets[engineModelID(id)]
	if !ok {
		return modelAdvice{Tier: "Local · unverified", Warning: "No memory profile yet. You can still select this model."}
	}
	target := b.Base + b.Reserve
	a := modelAdvice{Tier: memoryTier(target), Detail: fmt.Sprintf("Planning estimate: %d GB including %d GB for references and working memory. Not a measured minimum.", target, b.Reserve)}
	switch {
	case hw.Total <= 0:
		a.Warning = "Hardware unknown. Select to try; memory fit is unverified."
	case hw.DeviceType == "mps":
		a.Warning = "Shared-memory device: this discrete-GPU estimate may not apply. Select to try."
	case hw.Total < int64(target)*(1<<30):
		a.Dim = true
		a.Warning = fmt.Sprintf("May exceed your %.1f GiB GPU. Select anyway to override; lower resolution or offloading may help.", float64(hw.Total)/(1<<30))
	case hw.Free != nil && *hw.Free < int64(target)*(1<<30):
		a.Dim = true
		a.Warning = "Limited free VRAM right now. ComfyUI may reclaim cached models. Select anyway to override."
	default:
		a.Detail += " Within total VRAM; larger inputs can still run out of memory."
	}
	return a
}

// Cloud price bands compare like billing units only. Quality remains the
// separate, task-specific test rating; expensive never implies better quality.
func cloudAdvice(price float64, quality, unit string) modelAdvice {
	a := modelAdvice{Tier: "Cloud · price unknown", Detail: "Value cannot be ranked without a published price."}
	if price <= 0 {
		return a
	}
	switch {
	case price <= .05:
		a.Tier = "Cloud · Budget"
	case price <= .15:
		a.Tier = "Cloud · Standard"
	default:
		a.Tier = "Cloud · Premium cost"
	}
	q := "quality untested for this task"
	switch quality {
	case tierBest:
		q = "best in our task tests"
	case tierGood:
		q = "good in our task tests"
	case tierWeak:
		q = "weak in our task tests"
	}
	a.Detail = fmt.Sprintf("$%.3f/%s · %s. Price bands are advisory; see the full cost estimate.", price, unit, q)
	if price <= .05 && (quality == tierBest || quality == tierGood) {
		a.Tier = "Cloud · Value"
	}
	return a
}

// Featured placement is editorial; it never changes measured quality ratings.
func featuredModel(id string) string {
	switch {
	case strings.HasPrefix(id, "minimax-h3"):
		return "MiniMax H3"
	case strings.HasPrefix(id, "ltx"):
		return "LTX"
	case strings.HasPrefix(id, "flux"):
		return "Black Forest Labs"
	default:
		return ""
	}
}
