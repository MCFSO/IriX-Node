// 管理员账户来源网络限制：管理员只允许从内网地址登录与访问。
//
// 管理员（is_admin）拥有全部端点权限，一旦凭据在公网泄露，节点会被直接接管。
// 因此管理员账户的**登录**与**每一次会话访问**都要求来源地址属于内网：
//
//   - 环回：127.0.0.0/8、::1（节点默认监听 127.0.0.1，本机面板/CLI 必需）；
//   - RFC 1918 IPv4 私网：10.0.0.0/8、172.16.0.0/12、192.168.0.0/16；
//   - IPv6 内网等价段：fc00::/7（唯一本地地址 ULA）、fe80::/10（链路本地）。
//
// 其余来源一律拒绝，包括公网单播、CGNAT 100.64.0.0/10 与文档/测试网段。
// 普通账户（非管理员）不受此限制。
//
// 该限制没有配置开关：管理员权限默认只在本机与内网可用。跨公网的节点间
// 集群调用若使用管理员会话 token，会被同样的规则拒绝——跨公网集群请改用
// 普通账户并只授予集群端点权限。

package main

import "net"

// adminIntranetNets 管理员允许的来源网段（RFC 1918 + IPv6 内网等价段）。
// 环回地址在 adminIntranetAllowed 中单独放行（IsLoopback 覆盖 IPv4/IPv6）。
var adminIntranetNets = func() []*net.IPNet {
	cidrs := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"fc00::/7",
		"fe80::/10",
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, block, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		nets = append(nets, block)
	}
	return nets
}()

// adminIntranetAllowed 判断来源地址是否允许管理员账户访问。
// 空地址/无法解析的地址按「非内网」处理（fail-closed）。
func adminIntranetAllowed(remote string) bool {
	remote = stripIPv6Zone(remote)
	if remote == "" {
		return false
	}
	ip := net.ParseIP(remote)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, block := range adminIntranetNets {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// stripIPv6Zone 去掉 IPv6 地址的作用域后缀（如 fe80::1%eth0）。
func stripIPv6Zone(addr string) string {
	for i := 0; i < len(addr); i++ {
		if addr[i] == '%' {
			return addr[:i]
		}
	}
	return addr
}
