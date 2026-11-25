package api

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// protect はブラウザ経由の攻撃から API を守る。
//
//   - DNS リバインディング: localhost だけで待ち受けているとき、Host ヘッダーが localhost 以外なら拒否する。
//     (悪意のあるサイトのドメインを 127.0.0.1 に向けて、ブラウザから API を叩かせる攻撃)
//   - CSRF: 変更を伴うリクエストは、Origin ヘッダーがあれば Host と同じであること、
//     Sec-Fetch-Site ヘッダーがあれば same-origin か none であること。
//     ブラウザは別のサイトからのリクエストにこれらを必ず付ける。curl など付けないクライアントはそのまま通す。
func protect(next http.Handler, loopbackOnly bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if loopbackOnly && !isLoopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
				writeJSON(w, http.StatusForbidden, errorBody{Error: "cross-origin request is not allowed"})
				return
			}
			switch r.Header.Get("Sec-Fetch-Site") {
			case "", "same-origin", "none":
			default:
				writeJSON(w, http.StatusForbidden, errorBody{Error: "cross-site request is not allowed"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

func isLoopbackHost(hostport string) bool {
	h := hostOnly(hostport)
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

// IsLoopbackListen は待ち受けアドレスが loopback だけかを返す (例: 127.0.0.1:8080)。
func IsLoopbackListen(addr string) bool {
	return isLoopbackHost(addr)
}
