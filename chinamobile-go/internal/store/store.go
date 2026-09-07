package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// UsageItem 单项用量（已格式化，便于展示/推送）
type UsageItem struct {
	Used     string  `json:"used"`
	Total    string  `json:"total"`
	UsedNum  float64 `json:"used_num"`
	TotalNum float64 `json:"total_num"`
	Unit     string  `json:"unit"`
}

// QueryResult 一次查询的完整结果
type QueryResult struct {
	QueriedAt       string    `json:"queried_at"`
	City            string    `json:"city"`
	Balance         string    `json:"balance"`
	BalanceNum      float64   `json:"balance_num"`
	PlanName        string    `json:"plan_name"`
	RealtimeFee     string    `json:"realtime_fee"`
	BillTotal       string    `json:"bill_total"`
	BillReal        string    `json:"bill_real"`
	DiscountTotal   string    `json:"discount_total"`
	BillCycle       string    `json:"bill_cycle"`
	GeneralFlow     UsageItem `json:"general_flow"`
	SpecialFlow     UsageItem `json:"special_flow"`
	RegionalFlow    UsageItem `json:"regional_flow"`
	TotalFlow       UsageItem `json:"total_flow"`
	Voice           UsageItem `json:"voice"`
	VoiceRemaining  string    `json:"voice_remaining"`
	Sms             UsageItem `json:"sms"`
	SmsRemaining    string    `json:"sms_remaining"`
	FlowUsedPercent float64   `json:"flow_used_percent"` // 总流量已用百分比（告警用）
}

// Fields 输出/推送字段开关（17 项，与 Python 版一致）
type Fields struct {
	City            bool `json:"city"`
	Balance         bool `json:"balance"`
	PlanName        bool `json:"plan_name"`
	RealtimeFee     bool `json:"realtime_fee"`
	BillTotal       bool `json:"bill_total"`
	BillReal        bool `json:"bill_real"`
	DiscountTotal   bool `json:"discount_total"`
	BillCycle       bool `json:"bill_cycle"`
	GeneralFlow     bool `json:"general_flow"`
	SpecialFlow     bool `json:"special_flow"`
	RegionalFlow    bool `json:"regional_flow"`
	TotalFlow       bool `json:"total_flow"`
	VoiceUsed       bool `json:"voice_used"`
	VoiceRemaining  bool `json:"voice_remaining"`
	SmsUsed         bool `json:"sms_used"`
	SmsRemaining    bool `json:"sms_remaining"`
	QueryTime       bool `json:"query_time"`
}

// DefaultFields Python 版默认输出字段
func DefaultFields() Fields {
	return Fields{
		City:           true,
		Balance:        true,
		PlanName:       true,
		RealtimeFee:    true,
		BillTotal:      true,
		BillReal:       true,
		DiscountTotal:  true,
		BillCycle:      true,
		GeneralFlow:    true,
		SpecialFlow:    true,
		RegionalFlow:   true,
		TotalFlow:      true,
		VoiceUsed:      true,
		VoiceRemaining: true,
		SmsUsed:        true,
		SmsRemaining:   true,
		QueryTime:      true,
	}
}

// FieldLabels 字段中文标签（推送范围设置界面用）
func FieldLabels() []struct {
	Key   string
	Label string
} {
	return []struct {
		Key   string
		Label string
	}{
		{"city", "城市"}, {"balance", "话费余额"}, {"plan_name", "套餐名称"},
		{"realtime_fee", "实时话费"}, {"bill_total", "本月账单"}, {"bill_real", "实际应缴"},
		{"discount_total", "优惠合计"}, {"bill_cycle", "账单周期"},
		{"general_flow", "通用流量"}, {"special_flow", "定向流量"}, {"regional_flow", "区域流量"}, {"total_flow", "总流量"},
		{"voice_used", "语音已用"}, {"voice_remaining", "语音剩余"},
		{"sms_used", "短信已用"}, {"sms_remaining", "短信剩余"},
		{"query_time", "查询时间"},
	}
}

// PushSettings 推送设置
type PushSettings struct {
	BarkEnabled         bool    `json:"bark_enabled"`
	BarkKey             string  `json:"bark_key"`
	PushPlusEnabled     bool    `json:"pushplus_enabled"`
	PushPlusToken       string  `json:"pushplus_token"`
	Fields              Fields  `json:"fields"`
	AlertOnly           bool    `json:"alert_only"`
	AlertBalanceBelow   float64 `json:"alert_balance_below"`
	AlertFlowPercent    int     `json:"alert_flow_percent"`
}

// Settings 全局设置
type Settings struct {
	QueryTime string       `json:"query_time"` // 每日定时查询 HH:MM
	Push      PushSettings `json:"push"`
	TwoFA     struct {
		Channel string `json:"channel"` // "none" | "bark" | "pushplus"
	} `json:"twofa"`
}

// TwoFAEnabled 2FA 是否启用（选择了具体推送渠道）
func (s Settings) TwoFAEnabled() bool {
	return s.TwoFA.Channel == "bark" || s.TwoFA.Channel == "pushplus"
}

// User 面板登录用户
type User struct {
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"` // scrypt hex
	Salt         string    `json:"salt"`           // hex
	CreatedAt    time.Time `json:"created_at"`
}

// Account 移动账号
type Account struct {
	Phone           string       `json:"phone"`
	Remark          string       `json:"remark"`
	FieldsOverride  *Fields      `json:"fields_override,omitempty"` // 每号独立输出设置（nil=跟随全局）
	HasLoginState   bool          `json:"has_login_state"`          // user-data 目录是否存在
	LastQuery       time.Time    `json:"last_query"`
	LastOK          bool         `json:"last_ok"`
	LastError       string       `json:"last_error"`
	LastResult      *QueryResult `json:"last_result,omitempty"`
}

// EffectiveFields 账号生效字段：覆盖 > 全局
func (a *Account) EffectiveFields(global Fields) Fields {
	if a.FieldsOverride != nil {
		return *a.FieldsOverride
	}
	return global
}

// HistoryEntry 查询历史
type HistoryEntry struct {
	Time    string `json:"time"`
	Phone   string `json:"phone"`
	OK      bool   `json:"ok"`
	Summary string `json:"summary"`
	Error   string `json:"error,omitempty"`
}

// DailyRecord 每日快照：当天该号码最后一次成功查询的结果（费用明细页数据源）
type DailyRecord struct {
	Date   string       `json:"date"` // YYYY-MM-DD
	Phone  string       `json:"phone"`
	Result *QueryResult `json:"result"`
}

// Store 持久化根结构
type Store struct {
	mu       sync.Mutex `json:"-"`
	path     string     `json:"-"`
	Users    []User       `json:"users"`
	Settings Settings    `json:"settings"`
	Accounts []*Account   `json:"accounts"`
	History  []HistoryEntry `json:"history"`
	Daily    []DailyRecord  `json:"daily"`
}

const maxHistory = 200

const maxDailyDates = 400 // 每日快照保留最近 400 天

var phoneRe = regexp.MustCompile(`^1\d{10}$`)

// ValidPhone 校验手机号格式
func ValidPhone(p string) bool { return phoneRe.MatchString(p) }

// LoadStore 从 dataDir/store.json 加载，不存在则初始化默认值
func LoadStore(dataDir string) (*Store, error) {
	s := &Store{path: filepath.Join(dataDir, "store.json")}
	s.Settings.QueryTime = "08:00"
	s.Settings.Push.Fields = DefaultFields()
	s.Settings.Push.AlertBalanceBelow = 10
	s.Settings.Push.AlertFlowPercent = 80

	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := s.saveLocked(); err != nil {
				return nil, err
			}
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	if s.Settings.QueryTime == "" {
		s.Settings.QueryTime = "08:00"
	}
	// 兼容旧数据：JSON 里没有渠道开关字段时，填了 key 默认启用
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(b, &raw)
	if pushRaw, ok := raw["settings"]; ok {
		var pushMap map[string]json.RawMessage
		_ = json.Unmarshal(pushRaw, &pushMap)
		if p, ok := pushMap["push"]; ok {
			_ = json.Unmarshal(p, &pushMap)
			if _, ok := pushMap["bark_enabled"]; !ok && s.Settings.Push.BarkKey != "" {
				s.Settings.Push.BarkEnabled = true
			}
			if _, ok := pushMap["pushplus_enabled"]; !ok && s.Settings.Push.PushPlusToken != "" {
				s.Settings.Push.PushPlusEnabled = true
			}
		}
		// 兼容旧数据：2FA 旧版只有 enabled 开关，升级后按已配置渠道自动迁移
		if t, ok := pushMap["twofa"]; ok {
			var twofaMap map[string]json.RawMessage
			_ = json.Unmarshal(t, &twofaMap)
			if _, ok := twofaMap["channel"]; !ok {
				var old struct {
					Enabled bool `json:"enabled"`
				}
				_ = json.Unmarshal(t, &old)
				if old.Enabled {
					switch {
					case s.Settings.Push.BarkEnabled && s.Settings.Push.BarkKey != "":
						s.Settings.TwoFA.Channel = "bark"
					case s.Settings.Push.PushPlusEnabled && s.Settings.Push.PushPlusToken != "":
						s.Settings.TwoFA.Channel = "pushplus"
					}
				}
			}
		}
	}
	return s, nil
}

// Save 持久化
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

// Reload 从磁盘重新加载（备份导入后调用，原地更新以保持指针引用有效）
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	ns := &Store{}
	if err := json.Unmarshal(b, ns); err != nil {
		return err
	}
	s.Users = ns.Users
	s.Settings = ns.Settings
	s.Accounts = ns.Accounts
	s.History = ns.History
	s.Daily = ns.Daily
	return nil
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// HasUser 是否已初始化管理员
func (s *Store) HasUser() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Users) > 0
}

// CreateUser 创建管理员（仅允许一次）
func (s *Store) CreateUser(username, passwordHash, salt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Users) > 0 {
		return errors.New("管理员已存在")
	}
	s.Users = append(s.Users, User{
		Username:     username,
		PasswordHash: passwordHash,
		Salt:         salt,
		CreatedAt:    time.Now(),
	})
	return s.saveLocked()
}

// GetUser 按用户名查找
func (s *Store) GetUser(username string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.Users {
		if u.Username == username {
			return u, true
		}
	}
	return User{}, false
}

// UpdatePassword 更新密码
func (s *Store) UpdatePassword(username, passwordHash, salt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Users {
		if s.Users[i].Username == username {
			s.Users[i].PasswordHash = passwordHash
			s.Users[i].Salt = salt
			return s.saveLocked()
		}
	}
	return errors.New("用户不存在")
}

// GetSettings 读取设置
func (s *Store) GetSettings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Settings
}

// UpdateSettings 更新设置
func (s *Store) UpdateSettings(fn func(*Settings)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.Settings)
	return s.saveLocked()
}

// ListAccounts 账号列表（按手机号排序）
func (s *Store) ListAccounts() []*Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Account, len(s.Accounts))
	copy(out, s.Accounts)
	sort.Slice(out, func(i, j int) bool { return out[i].Phone < out[j].Phone })
	return out
}

// GetAccount 查找账号
func (s *Store) GetAccount(phone string) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getAccountLocked(phone)
}

func (s *Store) getAccountLocked(phone string) *Account {
	for _, a := range s.Accounts {
		if a.Phone == phone {
			return a
		}
	}
	return nil
}

// UpsertAccount 新增/更新账号基本信息
func (s *Store) UpsertAccount(phone, remark string) (*Account, error) {
	if !ValidPhone(phone) {
		return nil, errors.New("手机号格式不正确")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.getAccountLocked(phone)
	if a == nil {
		a = &Account{Phone: phone}
		s.Accounts = append(s.Accounts, a)
	}
	if remark != "" {
		a.Remark = remark
	}
	err := s.saveLocked()
	return a, err
}

// DeleteAccount 删除账号记录（连同每日快照）
func (s *Store) DeleteAccount(phone string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for i, a := range s.Accounts {
		if a.Phone == phone {
			s.Accounts = append(s.Accounts[:i], s.Accounts[i+1:]...)
			found = true
			break
		}
	}
	if found {
		// 同步删除该号码的每日快照
		out := s.Daily[:0]
		for _, d := range s.Daily {
			if d.Phone != phone {
				out = append(out, d)
			}
		}
		s.Daily = out
	}
	if found {
		_ = s.saveLocked()
	}
	return found
}

// UpdateAccount 更新账号（回调内修改）
func (s *Store) UpdateAccount(phone string, fn func(*Account)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.getAccountLocked(phone)
	if a == nil {
		return
	}
	fn(a)
	_ = s.saveLocked()
}

// AddHistory 追加查询历史（保留最近 200 条）
func (s *Store) AddHistory(e HistoryEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.History = append([]HistoryEntry{e}, s.History...)
	if len(s.History) > maxHistory {
		s.History = s.History[:maxHistory]
	}
	_ = s.saveLocked()
}

// ListHistory 查询历史
func (s *Store) ListHistory() []HistoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]HistoryEntry, len(s.History))
	copy(out, s.History)
	return out
}

// UpsertDaily 记录号码当日快照（同日重复查询取最后一次结果），按日期倒序保存
func (s *Store) UpsertDaily(phone string, res *QueryResult) {
	if res == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	date := time.Now().Format("2006-01-02")
	rec := DailyRecord{Date: date, Phone: phone, Result: res}
	for i, d := range s.Daily {
		if d.Date == date && d.Phone == phone {
			s.Daily[i] = rec
			s.trimDailyLocked()
			_ = s.saveLocked()
			return
		}
	}
	s.Daily = append(s.Daily, rec)
	sort.Slice(s.Daily, func(i, j int) bool {
		if s.Daily[i].Date != s.Daily[j].Date {
			return s.Daily[i].Date > s.Daily[j].Date
		}
		return s.Daily[i].Phone < s.Daily[j].Phone
	})
	s.trimDailyLocked()
	_ = s.saveLocked()
}

// trimDailyLocked 只保留最近 maxDailyDates 天的快照
func (s *Store) trimDailyLocked() {
	dates := map[string]bool{}
	for _, d := range s.Daily {
		dates[d.Date] = true
	}
	if len(dates) <= maxDailyDates {
		return
	}
	unique := make([]string, 0, len(dates))
	for d := range dates {
		unique = append(unique, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(unique)))
	cut := unique[maxDailyDates]
	out := s.Daily[:0]
	for _, d := range s.Daily {
		if d.Date > cut {
			out = append(out, d)
		}
	}
	s.Daily = out
}

// ListDaily 全部每日快照（日期倒序，同日按号码排序）
func (s *Store) ListDaily() []DailyRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DailyRecord, len(s.Daily))
	copy(out, s.Daily)
	return out
}

// RandomHex 生成 n 字节随机数的 hex
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// AccountsDir 账号数据根目录 data/accounts/<phone>
func AccountsDir(dataDir, phone string) string {
	return filepath.Join(dataDir, "accounts", phone)
}

// UserDataDir chromium 用户数据目录（登录态）
func UserDataDir(dataDir, phone string) string {
	return filepath.Join(AccountsDir(dataDir, phone), "user-data")
}
