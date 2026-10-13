package main

import (
	"net"
	"strings"
	"testing"
)

// These tests cover the decisions that happen before any network call. Every
// one of them is a place where a plausible implementation looks fine and is
// not: a registrar that boots happily and registers 127.0.0.1 is worse than one
// that refuses to start, because the failure surfaces as 502s on somebody else's
// dashboard.

func baseOptions() options {
	return options{
		server:      "http://goacos:8848",
		service:     "redclaw-console",
		port:        27094,
		advertiseIP: "100.106.177.3",
	}
}

func TestRegistryConfigRequiresTheThreeThingsThatCannotBeGuessed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*options)
		want   string
	}{
		{
			name:   "服务名缺失",
			mutate: func(o *options) { o.service = "" },
			want:   "-service",
		},
		{
			name:   "端口缺失",
			mutate: func(o *options) { o.port = 0 },
			want:   "-port",
		},
		{
			name:   "端口越界",
			mutate: func(o *options) { o.port = 70000 },
			want:   "-port",
		},
		{
			name:   "注册中心缺失——没有默认值可猜",
			mutate: func(o *options) { o.server = "" },
			want:   "-server",
		},
		{
			name:   "地址不是 IP",
			mutate: func(o *options) { o.advertiseIP = "not-an-ip" },
			want:   "is not an IP address",
		},
		{
			name:   "元数据不是 key=value",
			mutate: func(o *options) { o.meta = stringList{"role"} },
			want:   "not key=value",
		},
		{
			name:   "元数据键为空",
			mutate: func(o *options) { o.meta = stringList{"=frontend"} },
			want:   "not key=value",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := baseOptions()
			tc.mutate(&o)
			_, err := o.registryConfig()
			if err == nil {
				t.Fatal("配置错误竟然通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("报错没有指向真正的原因：%v（应包含 %q）", err, tc.want)
			}
		})
	}
}

// TestLoopbackIsRefusedByDefault is the invariant the whole fleet shares with
// the myvpn sidecar, ACC and RedClaw: default off, explicit opt-in.
func TestLoopbackIsRefusedByDefault(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "127.0.0.53", "::1"} {
		t.Run(addr, func(t *testing.T) {
			o := baseOptions()
			o.advertiseIP = addr
			_, err := o.registryConfig()
			if err == nil {
				t.Fatalf("%s 未开开关就被放行了 —— 在 mesh 里每个对端都把它解析到自己", addr)
			}
			if !strings.Contains(err.Error(), allowLoopbackEnv) {
				t.Fatalf("报错没有告诉运维怎么显式放行：%v", err)
			}
		})
	}
}

// 0.0.0.0 看着像「只在本机有意义」，其实它是 bind 通配符而不是目的地：
// 对端拨号过去哪儿也不到，单机如此，十机也如此。单机部署的正确答案是
// 127.0.0.1。这条与 sdk/registry.ConfigFromEnv 画同一条线。
func TestUnspecifiedIsRefusedEvenWithTheOptIn(t *testing.T) {
	for _, addr := range []string{"0.0.0.0", "::"} {
		t.Run(addr, func(t *testing.T) {
			o := baseOptions()
			o.advertiseIP = addr
			o.allowLoopback = true
			_, err := o.registryConfig()
			if err == nil {
				t.Fatalf("%s 在开关打开下竟被放行 —— 它不是可拨号地址", addr)
			}
		})
	}
}

func TestLoopbackOptInStillRejectsGarbage(t *testing.T) {
	// The failure this guards against: -allow-loopback implemented as "trust
	// whatever the operator typed". Being told the deployment is single-host
	// must not make a typo into a dialable endpoint.
	o := baseOptions()
	o.advertiseIP = "127.0.0.1.example.com"
	o.allowLoopback = true
	if _, err := o.registryConfig(); err == nil {
		t.Fatal("开关打开后垃圾字符串竟然通过了 —— 放行只应接受合法且只在本机有意义的地址")
	}
}

func TestLoopbackOptInAcceptsActuallyLocalAddresses(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "127.0.0.53", "::1"} {
		o := baseOptions()
		o.advertiseIP = addr
		o.allowLoopback = true
		if _, err := o.registryConfig(); err != nil {
			t.Fatalf("单机部署下 %s 显式放行却被拒：%v", addr, err)
		}
	}
}

func TestRoleBecomesMetadataAndFlagsSurvive(t *testing.T) {
	o := baseOptions()
	o.role = "frontend"
	o.meta = stringList{"tier=edge", "zone=mesh"}
	o.group = "WEB_GROUP"
	o.cluster = "SH"

	cfg, err := o.registryConfig()
	if err != nil {
		t.Fatalf("合法配置被拒：%v", err)
	}
	if cfg.Metadata["role"] != "frontend" {
		t.Errorf("role 元数据 = %q, want frontend", cfg.Metadata["role"])
	}
	if cfg.Metadata["tier"] != "edge" || cfg.Metadata["zone"] != "mesh" {
		t.Errorf("自定义元数据丢失：%v", cfg.Metadata)
	}
	if cfg.GroupName != "WEB_GROUP" || cfg.ClusterName != "SH" {
		t.Errorf("组/集群未透传：group=%q cluster=%q", cfg.GroupName, cfg.ClusterName)
	}
	if cfg.IP != "100.106.177.3" || cfg.Port != 27094 {
		t.Errorf("发布地址错了：%s:%d", cfg.IP, cfg.Port)
	}
	if !cfg.Enabled {
		t.Error("配置出来是关闭的")
	}
}

// TestWaitForDialRefusesToRegisterSomethingThatIsNotThere: the registrar's job
// is to publish a working endpoint, and a frontend that is not up yet must be
// waited for rather than advertised.
func TestWaitForDialRefusesToRegisterSomethingThatIsNotThere(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	defer ln.Close()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())

	// Something IS listening: this must succeed.
	if err := waitForDial("127.0.0.1", atoi(t, portStr), 3e9); err != nil {
		t.Fatalf("明明有东西在监听却等不到：%v", err)
	}

	ln.Close() // now nothing is
	if err := waitForDial("127.0.0.1", atoi(t, portStr), 1500e6); err == nil {
		t.Fatal("没有监听者竟然通过了 —— 那会把幽灵实例写进注册表")
	} else if !strings.Contains(err.Error(), "-verify-dial=false") {
		t.Errorf("报错没告诉运维可以显式跳过：%v", err)
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("端口 %q 不是纯数字", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}
