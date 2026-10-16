package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"wsdownloader/internal/config"
	"wsdownloader/server"
)

func Run(args []string, out, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(out, "WS下载器\n\n用法: ws-downloader <命令> [选项]\n  init    初始化 config.yaml\n  serve   启动 HTTP/WS 服务，TLS 由反向代理处理\n  add     添加本地文件或目录并通过 HTTP API 通知重载\n  list    列出配置中的分享资源\n  remove  移除分享资源并通知重载\n  reload  通过 HTTP API 重载配置\n\n每个命令支持 --help。CLI 可使用同样的管理命令。")
		return nil
	}
	name := args[0]
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(stderr)
	configPath := f.String("config", "config.yaml", "配置文件路径")
	var endpoint *string
	if name == "add" || name == "remove" || name == "reload" {
		endpoint = f.String("server", "", "临时覆盖管理地址，默认根据 admin_listen 生成本机 HTTP 地址")
	}
	// Accept both `add --config path dir` and the more natural `add dir --config path`.
	if name == "add" || name == "remove" {
		args = reorderPositionals(args)
	}
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	absoluteConfig, err := filepath.Abs(*configPath)
	if err != nil {
		return err
	}
	switch name {
	case "init":
		if f.NArg() != 0 {
			return errors.New("init 不接受位置参数")
		}
		return initialize(absoluteConfig, out)
	case "serve":
		if f.NArg() != 0 {
			return errors.New("serve 不接受位置参数")
		}
		cfg, err := config.Load(absoluteConfig)
		if err != nil {
			return fmt.Errorf("读取配置失败（首次使用请先执行 init）: %w", err)
		}
		app, err := server.New(absoluteConfig, cfg)
		if err != nil {
			return err
		}
		defer app.Close()
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return app.Serve(ctx)
	case "list":
		if f.NArg() != 0 {
			return errors.New("list 不接受位置参数")
		}
		cfg, err := config.Load(absoluteConfig)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(cfg.Resources)
	case "reload":
		if f.NArg() != 0 {
			return errors.New("reload 不接受位置参数")
		}
		cfg, err := config.Load(absoluteConfig)
		if err != nil {
			return err
		}
		return notify(cfg, *endpoint, out)
	case "add", "remove":
		if f.NArg() != 1 {
			return fmt.Errorf("%s 需要一个%s", name, map[string]string{"add": "本地文件/目录路径", "remove": "资源 ID"}[name])
		}
		unlock, err := config.Lock(absoluteConfig)
		if err != nil {
			return err
		}
		defer unlock()
		cfg, err := config.Load(absoluteConfig)
		if err != nil {
			return err
		}
		var resource config.Resource
		if name == "add" {
			p, err := filepath.Abs(f.Arg(0))
			if err != nil {
				return err
			}
			info, err := os.Stat(p)
			if err != nil {
				return err
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				return errors.New("只能添加文件或目录")
			}
			resource = config.Resource{ID: config.ID(p), Path: p}
			found := false
			for _, existing := range cfg.Resources {
				found = found || existing.ID == resource.ID
			}
			if !found {
				cfg.Resources = append(cfg.Resources, resource)
			}
		} else {
			index := -1
			for i, r := range cfg.Resources {
				if r.ID == f.Arg(0) {
					index, resource = i, r
					break
				}
			}
			if index == -1 {
				return errors.New("资源 ID 不存在")
			}
			cfg.Resources = append(cfg.Resources[:index], cfg.Resources[index+1:]...)
		}
		if err := config.Save(absoluteConfig, cfg); err != nil {
			return err
		}
		if err := json.NewEncoder(out).Encode(resource); err != nil {
			return err
		}
		if err := notify(cfg, *endpoint, stderr); err != nil {
			var networkErr net.Error
			if errors.As(err, &networkErr) {
				fmt.Fprintln(stderr, "配置已保存；无法连接服务，启动服务或执行 reload 后生效。", err)
				return nil
			}
			return fmt.Errorf("配置已保存，但重载失败: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("未知命令 %q；使用 --help 查看帮助", name)
	}
}

func reorderPositionals(args []string) []string {
	ordered := []string{args[0]}
	var positional []string
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") {
			ordered = append(ordered, arg)
			if !strings.Contains(arg, "=") && (arg == "--config" || arg == "-config" || arg == "--server" || arg == "-server") && i+1 < len(args) {
				i++
				ordered = append(ordered, args[i])
			}
		} else {
			positional = append(positional, arg)
		}
	}
	return append(append(ordered, "--"), positional...)
}

func notify(cfg config.Config, endpoint string, out io.Writer) error {
	if endpoint == "" {
		endpoint = cfg.AdminURL()
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("--server 必须是 HTTP/HTTPS 地址")
	}
	u.Path = "/api/reload"
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest(http.MethodPost, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	fmt.Fprintln(out, "服务端配置已重载。")
	return nil
}

func initialize(path string, out io.Writer) error {
	unlock, err := config.Lock(path)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(path); err == nil {
		return errors.New("配置已存在，init 不会覆盖")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cfg := config.Default()
	if err := config.Save(path, cfg); err != nil {
		return err
	}
	fmt.Fprintf(out, "WS下载器初始化完成。\n配置: %s\n本地页面: http://%s\n本机管理接口: %s\n公网 HTTPS/WSS 由 Caddy 等反向代理提供。\n", path, cfg.Listen, cfg.AdminURL())
	return nil
}
