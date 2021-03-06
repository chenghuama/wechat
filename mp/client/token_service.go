// @description wechat 是腾讯微信公众平台 api 的 golang 语言封装
// @link        https://github.com/chanxuehong/wechat for the canonical source repository
// @license     https://github.com/chanxuehong/wechat/blob/master/LICENSE
// @authors     chanxuehong(chanxuehong@gmail.com)

package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// access token 伺服接口, see token_service.png
type TokenService interface {
	// 获取 access token, 该 token 一般缓存在某个地方.
	// 正常情况下 token != "" && err == nil, 否则 token == "" && err != nil
	// NOTE: 该方法一定要功能上实现!
	Token() (token string, err error)

	// 从微信服务器获取新的 access token.
	// 正常情况下 token != "" && err == nil, 否则 token == "" && err != nil
	//  NOTE:
	//  1. 一般情况下无需调用该函数, 请使用 Token() 获取 access token.
	//  2. 该方法可以选择是否功能上实现, 如果没有需求可以在语法上实现即可!
	//  3. 即使微信服务器返回了 access token 过期错误(错误代码 42001, 正常情况下不会出现),
	//     也请谨慎调用 TokenRefresh, 建议直接返回错误! 因为很有可能高并发情况下造成雪崩效应!
	//  4. 再次强调, 调用这个函数你应该知道发生了什么!!!
	TokenRefresh() (token string, err error)
}

var _ TokenService = new(DefaultTokenService)

type DefaultTokenService struct {
	appid, appsecret string

	// goroutine tokenAutoUpdate() 里有个定时器, 每次触发都会更新 currentToken,
	// 同时 goroutine tokenAutoUpdate() 监听 resetTokenRefreshTickChan,
	// 如果有新的数据, 则重置定时器, 定时时间为 resetTokenRefreshTickChan 传过来的数据.
	currentToken struct {
		rwmutex sync.RWMutex
		token   string
		err     error
	}
	resetTokenRefreshTickChan chan time.Duration

	httpClient *http.Client
}

func NewDefaultTokenService(appid, appsecret string, httpClient *http.Client) (srv *DefaultTokenService) {
	srv = &DefaultTokenService{
		appid:                     appid,
		appsecret:                 appsecret,
		resetTokenRefreshTickChan: make(chan time.Duration),
	}

	if httpClient == nil {
		srv.httpClient = http.DefaultClient
	} else {
		srv.httpClient = httpClient
	}

	tk, err := srv.getNewToken()
	if err != nil {
		srv.currentToken.token = ""
		srv.currentToken.err = err
		go srv.tokenAutoUpdate(time.Minute) // 一分钟后尝试
	} else {
		srv.currentToken.token = tk.Token
		srv.currentToken.err = nil
		go srv.tokenAutoUpdate(time.Duration(tk.ExpiresIn) * time.Second)
	}
	return
}

func (srv *DefaultTokenService) Token() (token string, err error) {
	srv.currentToken.rwmutex.RLock()
	token = srv.currentToken.token
	err = srv.currentToken.err
	srv.currentToken.rwmutex.RUnlock()
	return
}

func (srv *DefaultTokenService) TokenRefresh() (token string, err error) {
	srv.currentToken.rwmutex.Lock()
	defer srv.currentToken.rwmutex.Unlock()

	resp, err := srv.getNewToken()
	if err != nil {
		srv.currentToken.token = ""
		srv.currentToken.err = err
		srv.resetTokenRefreshTickChan <- time.Minute // 一分钟后尝试
		return
	}

	token = resp.Token

	srv.currentToken.token = resp.Token
	srv.currentToken.err = nil
	srv.resetTokenRefreshTickChan <- time.Duration(resp.ExpiresIn) * time.Second
	return
}

// 从微信服务器获取 acces token 成功时返回的消息格式
type tokenResponse struct {
	Token     string `json:"access_token"` // 获取到的凭证
	ExpiresIn int64  `json:"expires_in"`   // 凭证有效时间，单位：秒
}

// 从微信服务器获取新的 access_token
func (srv *DefaultTokenService) getNewToken() (resp *tokenResponse, err error) {
	_url := "https://api.weixin.qq.com/cgi-bin/token?grant_type=client_credential&appid=" +
		srv.appid + "&secret=" + srv.appsecret

	httpResp, err := srv.httpClient.Get(_url)
	if err != nil {
		return
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		err = fmt.Errorf("http.Status: %s", httpResp.Status)
		return
	}

	var result struct {
		tokenResponse
		Error
	}
	if err = json.NewDecoder(httpResp.Body).Decode(&result); err != nil {
		return
	}

	if result.ErrCode != 0 {
		err = &result.Error
		return
	}

	// 由于网络的延时, access token 过期时间留了一个缓冲区;
	// 正常情况下微信服务器会返回 7200, 则缓冲区的大小为 10 分钟.
	switch {
	case result.ExpiresIn > 60*60: // 返回的过期时间大于 1 个小时, 缓冲区为 10 分钟
		result.ExpiresIn -= 60 * 10
		resp = &result.tokenResponse

	case result.ExpiresIn > 60*30: // 返回的过期时间大于 30 分钟, 缓冲区为 5 分钟
		result.ExpiresIn -= 60 * 5
		resp = &result.tokenResponse

	case result.ExpiresIn > 60*5: // 返回的过期时间大于 5 分钟, 缓冲区为 1 分钟
		result.ExpiresIn -= 60
		resp = &result.tokenResponse

	case result.ExpiresIn > 60: // 返回的过期时间大于 1 分钟, 缓冲区为 10 秒
		result.ExpiresIn -= 10
		resp = &result.tokenResponse

	case result.ExpiresIn > 0: // 没有办法了, 死马当做活马医了
		resp = &result.tokenResponse

	default:
		err = fmt.Errorf("expires_in 应该是正整数, 现在为: %d", result.ExpiresIn)
	}
	return
}

// 单独一个 goroutine 来定时获取 access token
func (srv *DefaultTokenService) tokenAutoUpdate(tickDuration time.Duration) {
	const defaultTickDuration = time.Minute // 设置 44 秒以上就不会超过限制(2000次/日)
	var ticker *time.Ticker

NEW_TICK_DURATION:
	ticker = time.NewTicker(tickDuration)
	for {
		select {
		case tickDuration = <-srv.resetTokenRefreshTickChan:
			ticker.Stop()
			goto NEW_TICK_DURATION

		case <-ticker.C:
			srv.currentToken.rwmutex.Lock()

			resp, err := srv.getNewToken()
			if err != nil {
				srv.currentToken.token = ""
				srv.currentToken.err = err

				srv.currentToken.rwmutex.Unlock()

				if tickDuration != defaultTickDuration { // 出错则重置到 defaultTickDuration
					ticker.Stop()
					tickDuration = defaultTickDuration
					goto NEW_TICK_DURATION
				}

			} else {
				srv.currentToken.token = resp.Token
				srv.currentToken.err = nil

				srv.currentToken.rwmutex.Unlock()

				newTickDuration := time.Duration(resp.ExpiresIn) * time.Second
				if tickDuration != newTickDuration {
					ticker.Stop()
					tickDuration = newTickDuration
					goto NEW_TICK_DURATION
				}
			}
		}
	}
}
