package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wssz527/kimiwork-plus/internal/bridge"
)

type trayAccounts struct {
	State            string                  `json:"state"`
	Accounts         []bridge.AccountSummary `json:"accounts"`
	CurrentID        string                  `json:"currentId"`
	Adding           bool                    `json:"adding"`
	RestoreAccountID string                  `json:"restoreAccountId"`
}

type accountTrayClient struct {
	dir    string
	client *http.Client
}

func (c accountTrayClient) request(ctx context.Context, method, path string, input any, output any) error {
	endpoint, err := bridge.ReadEndpoint(c.dir)
	if err != nil {
		return errors.New("account_service_unavailable")
	}
	var body []byte
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.URL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+endpoint.Key)
	if method == http.MethodPost {
		req.Header.Set("Origin", endpoint.URL)
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.client.Do(req)
	if err != nil {
		return errors.New("account_service_unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return errors.New("account_action_rejected")
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return errors.New("account_service_invalid_response")
	}
	if output != nil && json.Unmarshal(raw, output) != nil {
		return errors.New("account_service_invalid_response")
	}
	return nil
}

func installAccountTray(app *application.App, tray *application.SystemTray, menu *application.Menu, dir string, showAccounts func()) {
	client := accountTrayClient{dir: dir, client: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	accounts := menu.AddSubmenu("切换 Kimi 账号")
	accounts.Add("打开账号管理").OnClick(func(*application.Context) { showAccounts() })
	var loading atomic.Bool
	open := func() {
		if !loading.CompareAndSwap(false, true) {
			return
		}
		go func() {
			var response struct {
				Accounts trayAccounts `json:"accounts"`
			}
			err := client.request(context.Background(), http.MethodGet, "/api/accounts", nil, &response)
			application.InvokeAsync(func() {
				defer loading.Store(false)
				accounts.Clear()
				if err != nil || response.Accounts.State != "ready" {
					accounts.Add("账号状态暂不可读").SetEnabled(false)
				} else if len(response.Accounts.Accounts) == 0 {
					accounts.Add("尚未保存账号").SetEnabled(false)
				} else {
					for _, row := range response.Accounts.Accounts {
						id := row.ID
						label := row.Name
						if row.NeedsLogin {
							label += " · 需重新登录"
						}
						item := accounts.AddCheckbox(label, row.ID == response.Accounts.CurrentID)
						item.SetEnabled(row.ID != response.Accounts.CurrentID && !response.Accounts.Adding && response.Accounts.RestoreAccountID == "")
						item.OnClick(func(*application.Context) {
							showAccounts()
							go func() {
								err := client.request(context.Background(), http.MethodPost, "/api/actions", map[string]any{"action": "account-switch", "accountId": id}, nil)
								if err != nil {
									application.InvokeAsync(func() {
										app.Dialog.Error().SetTitle("账号切换未开始").SetMessage("本机后台未接受切换请求。请在账号管理中查看当前状态后重试。").Show()
									})
								}
							}()
						})
					}
				}
				accounts.AddSeparator()
				accounts.Add("打开账号管理").OnClick(func(*application.Context) { showAccounts() })
				menu.Update()
				tray.OpenMenu()
			})
		}()
	}
	tray.OnClick(open)
	tray.OnRightClick(open)
}
