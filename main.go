package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/mobile"
	"chinamobile-monitor/internal/push"
	"chinamobile-monitor/internal/runner"
	"chinamobile-monitor/internal/scheduler"
	"chinamobile-monitor/internal/store"
	_ "chinamobile-monitor/internal/telecom" // 注册中国电信 Provider
	_ "chinamobile-monitor/internal/unicom"  // 注册中国联通 Provider
	"chinamobile-monitor/internal/web"
)

const version = "1.0.0"

func main() {
	portDefault := 10086
	if v := strings.TrimSpace(os.Getenv("PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			portDefault = n
		}
	}
	dataDir := flag.String("data", envOr("DATA_DIR", "./data/chinamobile"), "数据目录（账号登录态/配置/日志）")
	port := flag.Int("port", portDefault, "Web 面板端口")
	loginCmd := flag.String("login", "", "本地登录指定手机号（弹出浏览器窗口）")
	queryCmd := flag.String("query", "", "命令行查询（可传 * 查询全部账号）")
	saveJSON := flag.Bool("json", false, "查询时保存原始响应 JSON")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		fmt.Println("创建数据目录失败:", err)
		os.Exit(1)
	}

	log := loggerx.New(*dataDir)
	// 清理上次异常退出残留的浏览器进程（会锁住登录态目录）
	mobile.KillStaleBrowsers(*dataDir)
	st, err := store.LoadStore(*dataDir)
	if err != nil {
		fmt.Println("加载配置失败:", err)
		os.Exit(1)
	}
	r := runner.New(*dataDir, st, log)

	switch {
	case *loginCmd != "":
		cliLogin(*dataDir, *loginCmd, log, st)
	case *queryCmd != "":
		cliQuery(*dataDir, *queryCmd, *saveJSON, log, st, r)
	default:
		serve(*dataDir, *port, st, log, r)
	}
}

func serve(dataDir string, port int, st *store.Store, log *loggerx.Logger, r *runner.Runner) {
	fmt.Printf("三网话费监控 v%s\n", version)
	if abs, err := filepath.Abs(dataDir); err == nil {
		fmt.Printf("数据目录: %s\n", abs)
	}

	// 首次启动时把已有账号的登录态标志同步一次（浏览器型看目录；电信看 token；联通看 Cookie）
	for _, a := range st.ListAccounts() {
		_, dirErr := os.Stat(store.UserDataDir(dataDir, a.Phone))
		if dirErr == nil && a.CarrierCode() == carrier.Mobile {
			st.UpdateAccount(a.Phone, func(x *store.Account) { x.HasLoginState = true })
		} else if a.CarrierCode() == carrier.Telecom && a.Token != "" {
			st.UpdateAccount(a.Phone, func(x *store.Account) { x.HasLoginState = true })
		} else if a.CarrierCode() == carrier.Unicom && a.Cookie != "" {
			st.UpdateAccount(a.Phone, func(x *store.Account) { x.HasLoginState = true })
		}
	}

	sch := scheduler.New(st, r, log)
	sch.Start()

	srv := web.New(st, log, r, port, dataDir)
	if err := srv.Start(); err != nil {
		log.Error("Web 服务退出: %v", err)
		os.Exit(1)
	}
}

// cliLogin 本地有头浏览器登录（与 Python 版 --login 行为一致）
func cliLogin(dataDir, phone string, log *loggerx.Logger, st *store.Store) {
	if !store.ValidPhone(phone) {
		fmt.Println("手机号格式不正确")
		os.Exit(1)
	}
	fmt.Println(strings.Repeat("=", 50))
	fmt.Printf("中国移动登录 → %s\n", phone)
	fmt.Println(strings.Repeat("=", 50))

	flow, err := mobile.StartLogin(phone, dataDir, false, log)
	if err != nil {
		fmt.Println("启动登录失败:", err)
		os.Exit(1)
	}

	// 终端输入验证码（也可直接在浏览器中操作，脚本会自动监测）
	go func() {
		reader := bufio.NewReader(os.Stdin)
		for {
			fmt.Print("\n请输入验证码后按回车（直接回车跳过）: ")
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			code := strings.TrimSpace(line)
			if code == "" {
				continue
			}
			_ = flow.SubmitCode(code)
		}
	}()

	<-flow.Done()
	stage, msg := flow.Status()
	fmt.Println()
	switch stage {
	case mobile.StageSuccess:
		fmt.Println("登录成功！")
		fmt.Printf("登录态已保存到: %s\n", store.UserDataDir(dataDir, phone))
		fmt.Println("下次查询时自动复用该状态。")
		st.UpdateAccount(phone, func(a *store.Account) { a.HasLoginState = true })
	default:
		fmt.Printf("登录结束: %s %s\n", mobile.StageText(stage), msg)
	}
}

// cliQuery 命令行查询（与 Python 版 --query 行为一致，含推送）
func cliQuery(dataDir, target string, saveJSON bool, log *loggerx.Logger, st *store.Store, r *runner.Runner) {
	accounts := st.ListAccounts()
	var phones []string
	if target == "*" || target == "all" {
		for _, a := range accounts {
			phones = append(phones, a.Phone)
		}
	} else {
		phones = strings.Split(target, ",")
	}
	if len(phones) == 0 {
		fmt.Println("配置文件中没有手机号，请先登录: chinamobile-go.exe --login 手机号")
		return
	}

	fmt.Printf("开始查询 %d 个号码...\n", len(phones))
	settings := st.GetSettings()

	var results []*carrier.Result
	for _, phone := range phones {
		phone = strings.TrimSpace(phone)
		if phone == "" {
			continue
		}
		fmt.Printf("\n→ 查询 %s\n", phone)
		pr := queryOne(dataDir, phone, saveJSON, log, st)
		results = append(results, pr)
		if pr.Err != "" {
			fmt.Printf("  [失败] %s\n", pr.Err)
		} else {
			fields := store.DefaultFields()
			if a := st.GetAccount(phone); a != nil {
				fields = a.EffectiveFields(settings.Push.Fields)
			}
			for _, line := range carrier.FormatResultLines(phone, pr.Result, fields) {
				fmt.Println(line)
			}
		}
	}

	fmt.Println("\n" + strings.Repeat("-", 50))
	output := carrier.FormatOutput(results, func(phone string) store.Fields {
		if a := st.GetAccount(phone); a != nil {
			return a.EffectiveFields(settings.Push.Fields)
		}
		return settings.Push.Fields
	})
	fmt.Println(output)

	// 推送（与 Python 版一致：查询后推送到已配置渠道）
	if push.HasChannel(settings.Push) {
		cfg := settings.Push
		if cfg.AlertOnly {
			alerts := []*carrier.Result{}
			for _, pr := range results {
				if pr.Err == "" && carrier.IsAlert(pr.Result, cfg.AlertBalanceBelow, cfg.AlertFlowPercent) {
					alerts = append(alerts, pr)
				}
			}
			if len(alerts) == 0 {
				log.Info("仅告警推送：本次无告警，跳过")
			} else {
				n := push.SendAll(cfg, "【套餐监控·告警】", carrier.FormatOutput(alerts, func(string) store.Fields { return cfg.Fields }))
				log.Info("告警推送完成（发送渠道数 %d）", n)
			}
		} else {
			n := push.SendAll(cfg, "【三网套餐用量监控】", output)
			log.Info("查询结果推送完成（发送渠道数 %d）", n)
		}
	}
}

func queryOne(dataDir, phone string, saveJSON bool, log *loggerx.Logger, st *store.Store) *carrier.Result {
	acc := st.GetAccount(phone)
	if acc == nil {
		return &carrier.Result{Phone: phone, Err: "账号不存在"}
	}
	pr := carrier.Get(acc.CarrierCode()).Query(acc, dataDir, log)
	now := time.Now()
	if pr.Err != "" {
		st.UpdateAccount(phone, func(a *store.Account) {
			a.LastQuery = now
			a.LastOK = false
			a.LastError = pr.Err
		})
		st.AddHistory(store.HistoryEntry{Time: now.Format("2006-01-02 15:04:05"), Phone: phone, OK: false, Error: pr.Err})
	} else {
		st.UpdateAccount(phone, func(a *store.Account) {
			a.LastQuery = now
			a.LastOK = true
			a.LastError = ""
			a.LastResult = pr.Result
			a.HasLoginState = true
		})
		st.AddHistory(store.HistoryEntry{Time: now.Format("2006-01-02 15:04:05"), Phone: phone, OK: true,
			Summary: fmt.Sprintf("余额 %s｜套餐 %s", pr.Result.Balance, pr.Result.PlanName)})
	}
	return pr
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
