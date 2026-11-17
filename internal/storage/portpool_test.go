package storage

import "testing"

// 端口契约的判据。
//
// 这些用例钉住的是一个**取舍**：注册面把端口存在实例行里，所以同一个服务名
// 下的实例**可以**落在不同端口上（模型就是这么定义的）。网关不能因此就把
// 服务判死，但必须能把「一个名字跨了多个端口」这件事说出来 —— 它是
// 「两个服务共用了同一个名字」的签名，在其它地方完全看不见。
//
// 三条判据：
//  1. 同构（所有实例同一个端口）⇒ 全量可用，无冲突；
//  2. 异构 + 组件声明了 servicePort ⇒ 只用声明端口的实例，其余排除并计数；
//  3. 异构 + 组件没声明 ⇒ 全量仍可用（模型语义不变），但冲突必须被报出来。

func instOn(service string, ip string, port uint32) Instance {
	return Instance{
		NamespaceId: "public",
		GroupName:   "DEFAULT_GROUP",
		ServiceName: service,
		ClusterName: "DEFAULT",
		IP:          ip,
		Port:        port,
		Weight:      1,
		Healthy:     true,
		Enabled:     true,
	}
}

// 1. 同构 ⇒ 全量可用，没有冲突。
func TestSelectPortPoolKeepsHomogeneousSetWhole(t *testing.T) {
	in := []Instance{
		instOn("memora-api", "10.0.0.1", 8091),
		instOn("memora-api", "10.0.0.2", 8091),
	}
	pool, conflict := SelectPortPool(in, 0)
	if conflict != nil {
		t.Fatalf("同构候选集不应产生冲突：%s", conflict.Reason())
	}
	if len(pool) != 2 {
		t.Fatalf("同构候选集应全部可用，实际 %d", len(pool))
	}
}

// 2. 声明了 servicePort ⇒ 只用该端口，其余排除并计数。
func TestDeclaredServicePortNarrowsToThatPort(t *testing.T) {
	in := []Instance{
		instOn("openpocket", "10.0.0.1", 4175),
		instOn("openpocket", "10.0.0.2", 8090),
	}
	pool, conflict := SelectPortPool(in, 8090)
	if len(pool) != 1 {
		t.Fatalf("声明端口后只应保留该端口的实例，实际 %d", len(pool))
	}
	if pool[0].Port != 8090 {
		t.Fatalf("保留的实例端口不对：%d", pool[0].Port)
	}
	if conflict == nil {
		t.Fatal("排除实例必须被记成冲突，否则这条排除没有理由")
	}
	if conflict.Declared != 8090 || conflict.Chosen != 8090 {
		t.Fatalf("冲突必须记住声明与选择：declared=%d chosen=%d", conflict.Declared, conflict.Chosen)
	}
	if conflict.Excluded != 1 {
		t.Fatalf("被排除的实例数不对：%d", conflict.Excluded)
	}
	if len(conflict.Ports) != 2 {
		t.Fatalf("冲突必须列出全部端口：%v", conflict.Ports)
	}
}

// 3. 没声明 ⇒ 全量仍可用（模型语义不变），但冲突必须被报出来。
//
// 这一条是整个文件里最容易被后来人「顺手改成 fail closed」的地方，所以它
// 单独成一条用例：如果哪天真的决定要 fail closed，这条用例会先红。
func TestUndeclaredPortSpreadStaysUsableButIsReported(t *testing.T) {
	in := []Instance{
		instOn("redclaw", "10.0.0.1", 27000),
		instOn("redclaw", "10.0.0.2", 27001),
	}
	pool, conflict := SelectPortPool(in, 0)
	if len(pool) != 2 {
		t.Fatalf("未声明端口时不应丢实例（命名模型按实例自带端口拨号），实际 %d", len(pool))
	}
	if conflict == nil {
		t.Fatal("端口跨布必须被报出来，否则它与「实例全挂」不可区分")
	}
	if len(conflict.Ports) != 2 || conflict.Ports[0] != 27000 || conflict.Ports[1] != 27001 {
		t.Fatalf("冲突端口必须升序列出：%v", conflict.Ports)
	}
	if conflict.Declared != 0 {
		t.Fatalf("未声明时 declared 必须为 0，实际 %d", conflict.Declared)
	}
}

// 3b. 声明了但一个实例都没有 ⇒ 池为空（fail closed），且冲突说明它。
//
// 这一条是「声明端口」与「不声明」的本质差别：声明是契约，契约落空就是
// 没有可用落点，必须让调用方看见，而不是悄悄退回混合池。
func TestDeclaredPortWithNoInstancesYieldsEmptyPool(t *testing.T) {
	in := []Instance{instOn("llm-gateway", "10.0.0.1", 8782)}
	pool, conflict := SelectPortPool(in, 8781)
	if len(pool) != 0 {
		t.Fatalf("声明端口无实例时池必须为空，实际 %d", len(pool))
	}
	if conflict == nil {
		t.Fatal("声明端口落空必须报冲突")
	}
	if conflict.Excluded != 1 {
		t.Fatalf("落空的实例数必须被计入排除：%d", conflict.Excluded)
	}
}

// 4. 声明值的解析：严格，空值/非数字/越界一律不算声明。
func TestDeclaredServicePortParsing(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want uint32
	}{
		{"plain", "8090", 8090},
		{"padded", " 8090 ", 8090},
		{"empty", "", 0},
		{"not a number", "eight-thousand", 0},
		{"zero", "0", 0},
		{"too large", "65536", 0},
		{"negative", "-1", 0},
		{"float", "8090.5", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DeclaredServicePort(map[string]string{ServicePortMetaKey: c.raw})
			if got != c.want {
				t.Fatalf("声明端口 %q 解析为 %d，期望 %d", c.raw, got, c.want)
			}
		})
	}
	if got := DeclaredServicePort(nil); got != 0 {
		t.Fatalf("无 metadata 时必须为 0，实际 %d", got)
	}
	if got := DeclaredServicePort(map[string]string{"other": "8090"}); got != 0 {
		t.Fatalf("只有 servicePort 键算数，实际 %d", got)
	}
}

// 5. 写路径的校验必须拒绝非法端口 —— 脏数据不能进路由表。
func TestParseServicePortRejectsBadValues(t *testing.T) {
	for _, raw := range []string{"", "abc", "0", "65536", "-5", "80.5"} {
		if _, err := ParseServicePort(raw); err == nil {
			t.Fatalf("非法端口 %q 必须被拒绝", raw)
		}
	}
	if p, err := ParseServicePort("4175"); err != nil || p != 4175 {
		t.Fatalf("合法端口必须通过：%d %v", p, err)
	}
}

// 6. 空候选集 ⇒ 没有冲突（没有实例与「实例跨端口」是两件事）。
func TestEmptyCandidateSetHasNoConflict(t *testing.T) {
	pool, conflict := SelectPortPool(nil, 8090)
	if len(pool) != 0 {
		t.Fatalf("空候选集应保持为空，实际 %d", len(pool))
	}
	if conflict != nil {
		t.Fatalf("空候选集不应产生端口冲突：%s", conflict.Reason())
	}
}
