package carrier

import "sync"

var (
	regMu    sync.RWMutex
	registry = map[string]Provider{}
	order    []string // 注册顺序（All 稳定输出）
)

// Register 注册运营商实现（各实现包 init() 调用）
func Register(p Provider) {
	regMu.Lock()
	defer regMu.Unlock()
	code := p.Code()
	if _, exists := registry[code]; !exists {
		order = append(order, code)
	}
	registry[code] = p
}

// Get 按代码取 Provider；空串或未知值兜底返回 mobile（旧数据兼容）
func Get(code string) Provider {
	regMu.RLock()
	defer regMu.RUnlock()
	if p, ok := registry[code]; ok {
		return p
	}
	return registry[Mobile]
}

// All 全部已注册运营商（GET /api/carriers 数据源）
func All() []Provider {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Provider, 0, len(order))
	for _, code := range order {
		if p, ok := registry[code]; ok {
			out = append(out, p)
		}
	}
	return out
}
