package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wssz527/kimiwork-plus/internal/bridge"
)

func trayAccountLabel(row bridge.AccountSummary) string {
	name := strings.TrimSpace(row.Name)
	if name == "" {
		var alnum strings.Builder
		for _, r := range row.ID {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				alnum.WriteRune(r)
			}
		}
		cleaned := alnum.String()
		if len(cleaned) > 6 {
			cleaned = cleaned[len(cleaned)-6:]
		}
		if cleaned != "" {
			name = "账号 " + cleaned + "（默认名）"
		} else {
			name = "未命名账号"
		}
	}
	if row.NeedsLogin {
		name += " · 需重新登录"
	}
	return name
}

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
	raw, err := io.ReadAll(io.LimitReader(res.Body, trayResponseLimit+1))
	if err != nil || len(raw) > trayResponseLimit {
		return errors.New("account_service_invalid_response")
	}
	if output != nil && json.Unmarshal(raw, output) != nil {
		return errors.New("account_service_invalid_response")
	}
	return nil
}

type trayAccountsView struct {
	app    *application.App
	menu   *application.Menu
	client accountTrayClient
	show   func()
}

func (v *trayAccountsView) refresh(state trayAccounts, err error) {
	v.menu.Clear()
	if err != nil || state.State != "ready" {
		v.menu.Add("账号状态暂不可读").SetEnabled(false)
	} else if len(state.Accounts) == 0 {
		v.menu.Add("尚未保存账号").SetEnabled(false)
	} else {
		for _, row := range state.Accounts {
			id := row.ID
			label := trayAccountLabel(row)
			item := v.menu.AddCheckbox(label, row.ID == state.CurrentID)
			item.SetEnabled(row.ID != state.CurrentID && !state.Adding && state.RestoreAccountID == "")
			item.OnClick(func(*application.Context) {
				if v.show != nil {
					v.show()
				}
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), trayAccountActionTimeout)
					defer cancel()
					err := v.client.request(ctx, http.MethodPost, "/api/actions", map[string]any{"action": "account-switch", "accountId": id}, nil)
					if err != nil {
						application.InvokeAsync(func() {
							v.app.Dialog.Error().SetTitle("账号切换未开始").SetMessage("本机后台未接受切换请求。请在账号管理中查看当前状态后重试。").Show()
						})
					}
				}()
			})
		}
	}
	v.menu.AddSeparator()
	v.menu.Add("打开账号管理").OnClick(func(*application.Context) {
		if v.show != nil {
			v.show()
		}
	})
}
