package option

import (
	"encoding/json"
	"testing"
)

// cache_runtime 的默认值是 true，但 Go 里用 *bool 表达，缺省与显式 false 必须能区分开。
// 这条语义一旦回退（比如有人把字段改成 bool），服务重启就会重新付 WebAssembly 重编译的
// ~266MB 峰值与 15 秒，而这在 Linux 上看不出来——所以把它钉成测试。
func TestEasyTierLifecycleDefaults(t *testing.T) {
	t.Parallel()
	var empty EasyTierOutboundOptions
	if err := json.Unmarshal([]byte(`{"network_name":"n"}`), &empty); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if empty.Lazy {
		t.Error("缺省 lazy 必须为 false（保持启动即建实例的旧行为）")
	}
	if empty.CacheRuntime != nil {
		t.Error("缺省 cache_runtime 必须是 nil，交由构造方解释成 true")
	}

	var explicit EasyTierOutboundOptions
	if err := json.Unmarshal([]byte(`{"lazy":true,"cache_runtime":false}`), &explicit); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !explicit.Lazy {
		t.Error("lazy=true 没解析出来")
	}
	if explicit.CacheRuntime == nil || *explicit.CacheRuntime {
		t.Error("cache_runtime=false 必须解析成非 nil 的 false，否则关不掉运行时缓存")
	}

	var on EasyTierOutboundOptions
	if err := json.Unmarshal([]byte(`{"cache_runtime":true}`), &on); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if on.CacheRuntime == nil || !*on.CacheRuntime {
		t.Error("cache_runtime=true 必须解析成非 nil 的 true")
	}
}
