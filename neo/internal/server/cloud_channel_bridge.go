// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	executortool "matrix/executor/tool"
	"matrix/neo/internal/channelgateway"
	"matrix/neo/internal/cloudchannels"
)

type cloudChannelStatus struct {
	Available         bool                        `json:"available"`
	Configured        bool                        `json:"configured"`
	Enabled           bool                        `json:"enabled"`
	Mode              string                      `json:"mode,omitempty"`
	AccountID         string                      `json:"account_id,omitempty"`
	BotUserID         string                      `json:"bot_user_id,omitempty"`
	Runtime           cloudchannels.RuntimeStatus `json:"runtime"`
	UnavailableReason string                      `json:"unavailable_reason,omitempty"`
}

type cloudChannelBridge struct {
	engine     *Engine
	store      *cloudchannels.Store
	storeErr   error
	slackAPI   *cloudchannels.SlackAPI
	discordAPI *cloudchannels.DiscordAPI
	larkAPI    *cloudchannels.LarkAPI
	dingAPI    *cloudchannels.DingTalkAPI
	wecomAPI   *cloudchannels.WeComAppAPI
	qqAPI      *cloudchannels.QQAPI
	weixinAPI  *cloudchannels.WeixinAPI
	wechatMP   *cloudchannels.WeChatMPAPI
	wechatKF   *cloudchannels.WeChatKFAPI
	slack      *cloudchannels.SlackSocket
	discord    *cloudchannels.DiscordGateway
	dingtalk   *cloudchannels.DingTalkStream
	wecomBot   *cloudchannels.WeComBotSocket
	lark       *cloudchannels.LarkSocket
	qq         *cloudchannels.QQGateway
	weixin     *cloudchannels.WeixinPoller

	mu       sync.Mutex
	rootCtx  context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	watching map[string]bool
	pending  map[string]cloudPending
	kfLocks  map[string]*sync.Mutex
}

type cloudPending struct {
	runID  string
	nodeID string
}

func newCloudChannelBridge(engine *Engine, store *cloudchannels.Store, storeErr error) *cloudChannelBridge {
	bridge := &cloudChannelBridge{
		engine: engine, store: store, storeErr: storeErr,
		slackAPI: cloudchannels.NewSlackAPI(), discordAPI: cloudchannels.NewDiscordAPI(), larkAPI: cloudchannels.NewLarkAPI(),
		dingAPI: cloudchannels.NewDingTalkAPI(), wecomAPI: cloudchannels.NewWeComAppAPI(), qqAPI: cloudchannels.NewQQAPI(), weixinAPI: cloudchannels.NewWeixinAPI(),
		wechatMP: cloudchannels.NewWeChatMPAPI(), wechatKF: cloudchannels.NewWeChatKFAPI(), watching: map[string]bool{}, pending: map[string]cloudPending{}, kfLocks: map[string]*sync.Mutex{},
	}
	if store != nil {
		bridge.slack = cloudchannels.NewSlackSocket(bridge.slackAPI, func() cloudchannels.SlackConfig { return store.View().Slack }, bridge.handleSlackPayload, bridge.handleSlackAction)
		bridge.discord = cloudchannels.NewDiscordGateway(bridge.discordAPI, store, func() cloudchannels.DiscordConfig { return store.View().Discord }, bridge.handleDiscordMessage)
		bridge.dingtalk = cloudchannels.NewDingTalkStream(func() cloudchannels.DingTalkConfig { return store.View().DingTalk }, bridge.handleDingTalkMessage)
		bridge.wecomBot = cloudchannels.NewWeComBotSocket(func() cloudchannels.WeComBotConfig { return store.View().WeComBot }, bridge.handleWeComBotMessage)
		bridge.lark = cloudchannels.NewLarkSocket(func() cloudchannels.LarkConfig { return store.View().Lark }, bridge.handleLarkPayload, bridge.handleLarkCardAction)
		bridge.qq = cloudchannels.NewQQGateway(bridge.qqAPI, store, func() cloudchannels.QQConfig { return store.View().QQ }, bridge.handleQQMessage)
		bridge.weixin = cloudchannels.NewWeixinPoller(bridge.weixinAPI, store, func() cloudchannels.WeixinConfig { return store.View().Weixin }, bridge.handleWeixinMessage)
	}
	return bridge
}

func (b *cloudChannelBridge) Start(ctx context.Context) {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.cancel != nil {
		b.mu.Unlock()
		return
	}
	runtimeCtx, cancel := context.WithCancel(ctx)
	b.rootCtx = runtimeCtx
	b.cancel = cancel
	b.done = make(chan struct{})
	done := b.done
	b.mu.Unlock()
	state := b.state()
	if state.Slack.Enabled && state.Slack.Mode == "socket" && b.slack != nil {
		b.slack.Start(runtimeCtx)
	}
	if state.Discord.Enabled && state.Discord.Gateway && b.discord != nil {
		b.discord.Start(runtimeCtx)
	}
	if state.DingTalk.Enabled && state.DingTalk.Mode == "stream" && b.dingtalk != nil {
		b.dingtalk.Start(runtimeCtx)
	}
	if state.WeComBot.Enabled && state.WeComBot.Mode == "websocket" && b.wecomBot != nil {
		b.wecomBot.Start(runtimeCtx)
	}
	if state.Lark.Enabled && state.Lark.Mode == "websocket" && b.lark != nil {
		b.lark.Start(runtimeCtx)
	}
	if state.QQ.Enabled && b.qq != nil {
		b.qq.Start(runtimeCtx)
	}
	if state.Weixin.Enabled && b.weixin != nil {
		b.weixin.Start(runtimeCtx)
	}
	go func() {
		defer close(done)
		b.drainLoop(runtimeCtx)
	}()
}

func (b *cloudChannelBridge) Stop() {
	if b == nil {
		return
	}
	b.mu.Lock()
	cancel, done := b.cancel, b.done
	b.cancel, b.done = nil, nil
	b.rootCtx = nil
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if b.slack != nil {
		b.slack.Stop()
	}
	if b.discord != nil {
		b.discord.Stop()
	}
	if b.dingtalk != nil {
		b.dingtalk.Stop()
	}
	if b.wecomBot != nil {
		b.wecomBot.Stop()
	}
	if b.lark != nil {
		b.lark.Stop()
	}
	if b.qq != nil {
		b.qq.Stop()
	}
	if b.weixin != nil {
		b.weixin.Stop()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
}

func (b *cloudChannelBridge) state() cloudchannels.State {
	if b == nil || b.store == nil {
		return cloudchannels.State{}
	}
	return b.store.View()
}

func (b *cloudChannelBridge) SlackStatus() cloudChannelStatus {
	if b == nil || b.store == nil {
		reason := "Slack cloud channel storage is unavailable"
		if b != nil && b.storeErr != nil {
			reason = b.storeErr.Error()
		}
		return cloudChannelStatus{UnavailableReason: reason}
	}
	config := b.store.View().Slack
	status := cloudChannelStatus{Available: true, Configured: config.Configured(), Enabled: config.Enabled, Mode: config.Mode, AccountID: config.TeamID, BotUserID: config.BotUserID}
	if b.slack != nil {
		status.Runtime = b.slack.Status()
		status.Runtime.LastError = safeCloudError(status.Runtime.LastError, config.BotToken, config.AppToken, config.SigningSecret)
	}
	return status
}

func (b *cloudChannelBridge) DiscordStatus() cloudChannelStatus {
	if b == nil || b.store == nil {
		reason := "Discord cloud channel storage is unavailable"
		if b != nil && b.storeErr != nil {
			reason = b.storeErr.Error()
		}
		return cloudChannelStatus{UnavailableReason: reason}
	}
	config := b.store.View().Discord
	status := cloudChannelStatus{Available: true, Configured: config.Configured(), Enabled: config.Enabled, Mode: "webhook", AccountID: config.ApplicationID, BotUserID: config.BotUserID}
	if config.Gateway {
		status.Mode = "gateway+webhook"
	}
	if b.discord != nil {
		status.Runtime = b.discord.Status()
		status.Runtime.LastError = safeCloudError(status.Runtime.LastError, config.BotToken)
	}
	return status
}

func (b *cloudChannelBridge) LarkStatus() cloudChannelStatus {
	if b == nil || b.store == nil {
		return cloudChannelStatus{UnavailableReason: "Lark cloud channel storage is unavailable"}
	}
	config := b.store.View().Lark
	status := cloudChannelStatus{Available: true, Configured: config.Configured(), Enabled: config.Enabled, Mode: config.Mode, AccountID: config.AppID, BotUserID: config.BotOpenID}
	if b.lark != nil {
		status.Runtime = b.lark.Status()
		status.Runtime.LastError = safeCloudError(status.Runtime.LastError, config.AppSecret, config.VerificationToken, config.EncryptKey)
	}
	return status
}

func (b *cloudChannelBridge) DingTalkStatus() cloudChannelStatus {
	if b == nil || b.store == nil {
		return cloudChannelStatus{UnavailableReason: "DingTalk cloud channel storage is unavailable"}
	}
	config := b.store.View().DingTalk
	status := cloudChannelStatus{Available: true, Configured: config.Configured(), Enabled: config.Enabled, Mode: config.Mode, AccountID: config.ClientID, BotUserID: config.RobotCode}
	if b.dingtalk != nil {
		status.Runtime = b.dingtalk.Status()
		status.Runtime.LastError = safeCloudError(status.Runtime.LastError, config.ClientSecret, config.CallbackSecret)
	}
	return status
}

func (b *cloudChannelBridge) WeComBotStatus() cloudChannelStatus {
	if b == nil || b.store == nil {
		return cloudChannelStatus{UnavailableReason: "WeCom Bot cloud channel storage is unavailable"}
	}
	config := b.store.View().WeComBot
	status := cloudChannelStatus{Available: true, Configured: config.Configured(), Enabled: config.Enabled, Mode: config.Mode, AccountID: config.BotID, BotUserID: config.BotID}
	if b.wecomBot != nil {
		status.Runtime = b.wecomBot.Status()
		status.Runtime.LastError = safeCloudError(status.Runtime.LastError, config.Secret, config.Token, config.EncodingAESKey)
	}
	return status
}

func (b *cloudChannelBridge) WeComAppStatus() cloudChannelStatus {
	if b == nil || b.store == nil {
		return cloudChannelStatus{UnavailableReason: "WeCom App cloud channel storage is unavailable"}
	}
	config := b.store.View().WeComApp
	return cloudChannelStatus{Available: true, Configured: config.Configured(), Enabled: config.Enabled, Mode: "callback", AccountID: config.CorpID, BotUserID: fmt.Sprint(config.AgentID)}
}

func (b *cloudChannelBridge) QQStatus() cloudChannelStatus {
	if b == nil || b.store == nil {
		return cloudChannelStatus{UnavailableReason: "QQ cloud channel storage is unavailable"}
	}
	config := b.store.View().QQ
	status := cloudChannelStatus{Available: true, Configured: config.Configured(), Enabled: config.Enabled, Mode: "gateway", AccountID: config.AppID, BotUserID: config.BotID}
	if b.qq != nil {
		status.Runtime = b.qq.Status()
		status.Runtime.LastError = safeCloudError(status.Runtime.LastError, config.ClientSecret)
	}
	return status
}
func (b *cloudChannelBridge) WeixinStatus() cloudChannelStatus {
	if b == nil || b.store == nil {
		return cloudChannelStatus{UnavailableReason: "Weixin cloud channel storage is unavailable"}
	}
	config := b.store.View().Weixin
	status := cloudChannelStatus{Available: true, Configured: config.Configured(), Enabled: config.Enabled, Mode: "long_poll", AccountID: config.BotID, BotUserID: config.BotID}
	if b.weixin != nil {
		status.Runtime = b.weixin.Status()
		status.Runtime.LastError = safeCloudError(status.Runtime.LastError, config.Token)
	}
	return status
}
func (b *cloudChannelBridge) WeChatMPStatus() cloudChannelStatus {
	if b == nil || b.store == nil {
		return cloudChannelStatus{UnavailableReason: "WeChat Official Account storage is unavailable"}
	}
	config := b.store.View().WeChatMP
	return cloudChannelStatus{Available: true, Configured: config.Configured(), Enabled: config.Enabled, Mode: "callback", AccountID: config.AppID, BotUserID: config.AppID}
}
func (b *cloudChannelBridge) WeChatKFStatus() cloudChannelStatus {
	if b == nil || b.store == nil {
		return cloudChannelStatus{UnavailableReason: "WeChat Customer Service storage is unavailable"}
	}
	config := b.store.View().WeChatKF
	return cloudChannelStatus{Available: true, Configured: config.Configured(), Enabled: config.Enabled, Mode: "callback+sync", AccountID: config.CorpID, BotUserID: config.CorpID}
}

func (b *cloudChannelBridge) ConfigureLark(ctx context.Context, config cloudchannels.LarkConfig) (cloudChannelStatus, error) {
	if b == nil || b.store == nil {
		return cloudChannelStatus{}, cloudchannels.ErrEncryptionRequired
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	botID, err := b.larkAPI.BotOpenID(probeCtx, config)
	if err != nil {
		return b.LarkStatus(), err
	}
	config.BotOpenID = botID
	if err := b.store.ReplaceLark(config); err != nil {
		return b.LarkStatus(), err
	}
	b.auditConfiguration(ctx, channelgateway.ChannelLark, config.AppID, config.Mode, config.Enabled)
	if b.lark != nil {
		b.lark.Stop()
		if config.Enabled && config.Mode == "websocket" {
			b.lark.Start(b.context())
		}
	}
	return b.LarkStatus(), nil
}

func (b *cloudChannelBridge) ConfigureDingTalk(ctx context.Context, config cloudchannels.DingTalkConfig) (cloudChannelStatus, error) {
	if b == nil || b.store == nil {
		return cloudChannelStatus{}, cloudchannels.ErrEncryptionRequired
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := b.dingAPI.AccessToken(probeCtx, config); err != nil {
		return b.DingTalkStatus(), err
	}
	if err := b.store.ReplaceDingTalk(config); err != nil {
		return b.DingTalkStatus(), err
	}
	b.auditConfiguration(ctx, channelgateway.ChannelDingTalk, config.ClientID, config.Mode, config.Enabled)
	if b.dingtalk != nil {
		b.dingtalk.Stop()
		if config.Enabled && config.Mode == "stream" {
			b.dingtalk.Start(b.context())
		}
	}
	return b.DingTalkStatus(), nil
}

func (b *cloudChannelBridge) ConfigureWeComBot(ctx context.Context, config cloudchannels.WeComBotConfig) (cloudChannelStatus, error) {
	if b == nil || b.store == nil {
		return cloudChannelStatus{}, cloudchannels.ErrEncryptionRequired
	}
	if config.Mode == "callback" {
		if _, err := cloudchannels.NewWeComCrypto(config.Token, config.EncodingAESKey, config.ReceiveID); err != nil {
			return b.WeComBotStatus(), err
		}
	}
	if err := b.store.ReplaceWeComBot(config); err != nil {
		return b.WeComBotStatus(), err
	}
	b.auditConfiguration(ctx, channelgateway.ChannelWeComBot, config.BotID, config.Mode, config.Enabled)
	if b.wecomBot != nil {
		b.wecomBot.Stop()
		if config.Enabled && config.Mode == "websocket" {
			b.wecomBot.Start(b.context())
		}
	}
	return b.WeComBotStatus(), nil
}

func (b *cloudChannelBridge) ConfigureWeComApp(ctx context.Context, config cloudchannels.WeComAppConfig) (cloudChannelStatus, error) {
	if b == nil || b.store == nil {
		return cloudChannelStatus{}, cloudchannels.ErrEncryptionRequired
	}
	if _, err := cloudchannels.NewWeComCrypto(config.Token, config.EncodingAESKey, config.CorpID); err != nil {
		return b.WeComAppStatus(), err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := b.wecomAPI.AccessToken(probeCtx, config); err != nil {
		return b.WeComAppStatus(), err
	}
	if err := b.store.ReplaceWeComApp(config); err != nil {
		return b.WeComAppStatus(), err
	}
	b.auditConfiguration(ctx, channelgateway.ChannelWeComApp, config.CorpID, "callback", config.Enabled)
	return b.WeComAppStatus(), nil
}

func (b *cloudChannelBridge) ConfigureQQ(ctx context.Context, config cloudchannels.QQConfig) (cloudChannelStatus, error) {
	if b == nil || b.store == nil {
		return cloudChannelStatus{}, cloudchannels.ErrEncryptionRequired
	}
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := b.qqAPI.AccessToken(probe, config); err != nil {
		return b.QQStatus(), err
	}
	if _, err := b.qqAPI.GatewayURL(probe, config); err != nil {
		return b.QQStatus(), err
	}
	previous := b.store.View().QQ
	if previous.AppID == config.AppID {
		config.SessionID, config.ResumeURL, config.Sequence = previous.SessionID, previous.ResumeURL, previous.Sequence
	}
	if err := b.store.ReplaceQQ(config); err != nil {
		return b.QQStatus(), err
	}
	b.auditConfiguration(ctx, channelgateway.ChannelQQ, config.AppID, "gateway", config.Enabled)
	if b.qq != nil {
		b.qq.Stop()
		if config.Enabled {
			b.qq.Start(b.context())
		}
	}
	return b.QQStatus(), nil
}
func (b *cloudChannelBridge) ConfigureWeixin(ctx context.Context, config cloudchannels.WeixinConfig) (cloudChannelStatus, error) {
	if b == nil || b.store == nil {
		return cloudChannelStatus{}, cloudchannels.ErrEncryptionRequired
	}
	previous := b.store.View().Weixin
	if previous.BotID == config.BotID && previous.Token == config.Token {
		config.UpdatesCursor, config.Contexts = previous.UpdatesCursor, previous.Contexts
	}
	if err := b.store.ReplaceWeixin(config); err != nil {
		return b.WeixinStatus(), err
	}
	b.auditConfiguration(ctx, channelgateway.ChannelWeixin, config.BotID, "long_poll", config.Enabled)
	if b.weixin != nil {
		b.weixin.Stop()
		if config.Enabled {
			b.weixin.Start(b.context())
		}
	}
	return b.WeixinStatus(), nil
}
func (b *cloudChannelBridge) ConfigureWeChatMP(ctx context.Context, config cloudchannels.WeChatMPConfig) (cloudChannelStatus, error) {
	if b == nil || b.store == nil {
		return cloudChannelStatus{}, cloudchannels.ErrEncryptionRequired
	}
	if config.EncodingAESKey != "" {
		if _, err := cloudchannels.NewWeComCrypto(config.Token, config.EncodingAESKey, config.AppID); err != nil {
			return b.WeChatMPStatus(), err
		}
	}
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := b.wechatMP.AccessToken(probe, config); err != nil {
		return b.WeChatMPStatus(), err
	}
	if err := b.store.ReplaceWeChatMP(config); err != nil {
		return b.WeChatMPStatus(), err
	}
	b.auditConfiguration(ctx, channelgateway.ChannelWeChatMP, config.AppID, "callback", config.Enabled)
	return b.WeChatMPStatus(), nil
}
func (b *cloudChannelBridge) ConfigureWeChatKF(ctx context.Context, config cloudchannels.WeChatKFConfig) (cloudChannelStatus, error) {
	if b == nil || b.store == nil {
		return cloudChannelStatus{}, cloudchannels.ErrEncryptionRequired
	}
	if _, err := cloudchannels.NewWeComCrypto(config.Token, config.EncodingAESKey, config.CorpID); err != nil {
		return b.WeChatKFStatus(), err
	}
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := b.wechatKF.AccessToken(probe, config); err != nil {
		return b.WeChatKFStatus(), err
	}
	previous := b.store.View().WeChatKF
	if previous.CorpID == config.CorpID {
		config.Cursors = previous.Cursors
	}
	if err := b.store.ReplaceWeChatKF(config); err != nil {
		return b.WeChatKFStatus(), err
	}
	b.auditConfiguration(ctx, channelgateway.ChannelWeChatKF, config.CorpID, "callback+sync", config.Enabled)
	return b.WeChatKFStatus(), nil
}

func (b *cloudChannelBridge) auditConfiguration(ctx context.Context, channel channelgateway.Channel, account, mode string, enabled bool) {
	if b.engine.channelGateway != nil {
		_ = b.engine.channelGateway.RecordAudit(ctx, channel, account, "configuration.replace", mode, "effect=network enabled="+fmt.Sprint(enabled))
	}
}

func (b *cloudChannelBridge) ConfigureSlack(ctx context.Context, config cloudchannels.SlackConfig) (cloudChannelStatus, error) {
	if b == nil || b.store == nil {
		return cloudChannelStatus{}, cloudchannels.ErrEncryptionRequired
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	auth, err := b.slackAPI.AuthTest(probeCtx, config.BotToken)
	if err != nil {
		return b.SlackStatus(), err
	}
	config.TeamID, config.BotUserID = auth.TeamID, auth.BotUserID
	if err := b.store.ReplaceSlack(config); err != nil {
		return b.SlackStatus(), err
	}
	if b.engine.channelGateway != nil {
		_ = b.engine.channelGateway.RecordAudit(ctx, channelgateway.ChannelSlack, config.TeamID, "configuration.replace", config.Mode, "effect=network enabled="+fmt.Sprint(config.Enabled))
	}
	if b.slack != nil {
		b.slack.Stop()
		if config.Enabled && config.Mode == "socket" {
			b.slack.Start(b.context())
		}
	}
	return b.SlackStatus(), nil
}

func (b *cloudChannelBridge) ConfigureDiscord(ctx context.Context, config cloudchannels.DiscordConfig) (cloudChannelStatus, error) {
	if b == nil || b.store == nil {
		return cloudChannelStatus{}, cloudchannels.ErrEncryptionRequired
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	identity, err := b.discordAPI.Identity(probeCtx, config.BotToken)
	if err != nil {
		return b.DiscordStatus(), err
	}
	config.BotUserID = identity.ID
	if err := b.store.ReplaceDiscord(config); err != nil {
		return b.DiscordStatus(), err
	}
	if b.engine.channelGateway != nil {
		_ = b.engine.channelGateway.RecordAudit(ctx, channelgateway.ChannelDiscord, config.ApplicationID, "configuration.replace", "gateway", "effect=network enabled="+fmt.Sprint(config.Enabled))
	}
	if b.discord != nil {
		b.discord.Stop()
		if config.Enabled && config.Gateway {
			b.discord.Start(b.context())
		}
	}
	return b.DiscordStatus(), nil
}

func (b *cloudChannelBridge) DisconnectSlack() error {
	if b == nil || b.store == nil {
		return nil
	}
	if b.slack != nil {
		b.slack.Stop()
	}
	config := b.store.View().Slack
	err := b.store.ClearSlack()
	if err == nil && b.engine.channelGateway != nil {
		_ = b.engine.channelGateway.RecordAudit(context.Background(), channelgateway.ChannelSlack, config.TeamID, "configuration.remove", "", "")
	}
	return err
}

func (b *cloudChannelBridge) DisconnectDiscord() error {
	if b == nil || b.store == nil {
		return nil
	}
	if b.discord != nil {
		b.discord.Stop()
	}
	config := b.store.View().Discord
	err := b.store.ClearDiscord()
	if err == nil && b.engine.channelGateway != nil {
		_ = b.engine.channelGateway.RecordAudit(context.Background(), channelgateway.ChannelDiscord, config.ApplicationID, "configuration.remove", "", "")
	}
	return err
}

func (b *cloudChannelBridge) DisconnectLark() error {
	if b == nil || b.store == nil {
		return nil
	}
	if b.lark != nil {
		b.lark.Stop()
	}
	config := b.store.View().Lark
	err := b.store.ClearLark()
	if err == nil {
		b.auditRemoval(channelgateway.ChannelLark, config.AppID)
	}
	return err
}

func (b *cloudChannelBridge) DisconnectDingTalk() error {
	if b == nil || b.store == nil {
		return nil
	}
	if b.dingtalk != nil {
		b.dingtalk.Stop()
	}
	config := b.store.View().DingTalk
	err := b.store.ClearDingTalk()
	if err == nil {
		b.auditRemoval(channelgateway.ChannelDingTalk, config.ClientID)
	}
	return err
}

func (b *cloudChannelBridge) DisconnectWeComBot() error {
	if b == nil || b.store == nil {
		return nil
	}
	if b.wecomBot != nil {
		b.wecomBot.Stop()
	}
	config := b.store.View().WeComBot
	err := b.store.ClearWeComBot()
	if err == nil {
		b.auditRemoval(channelgateway.ChannelWeComBot, config.BotID)
	}
	return err
}

func (b *cloudChannelBridge) DisconnectWeComApp() error {
	if b == nil || b.store == nil {
		return nil
	}
	config := b.store.View().WeComApp
	err := b.store.ClearWeComApp()
	if err == nil {
		b.auditRemoval(channelgateway.ChannelWeComApp, config.CorpID)
	}
	return err
}

func (b *cloudChannelBridge) DisconnectQQ() error {
	if b == nil || b.store == nil {
		return nil
	}
	if b.qq != nil {
		b.qq.Stop()
	}
	config := b.store.View().QQ
	err := b.store.ClearQQ()
	if err == nil {
		b.auditRemoval(channelgateway.ChannelQQ, config.AppID)
	}
	return err
}
func (b *cloudChannelBridge) DisconnectWeixin() error {
	if b == nil || b.store == nil {
		return nil
	}
	if b.weixin != nil {
		b.weixin.Stop()
	}
	config := b.store.View().Weixin
	err := b.store.ClearWeixin()
	if err == nil {
		b.auditRemoval(channelgateway.ChannelWeixin, config.BotID)
	}
	return err
}
func (b *cloudChannelBridge) DisconnectWeChatMP() error {
	if b == nil || b.store == nil {
		return nil
	}
	config := b.store.View().WeChatMP
	err := b.store.ClearWeChatMP()
	if err == nil {
		b.auditRemoval(channelgateway.ChannelWeChatMP, config.AppID)
	}
	return err
}
func (b *cloudChannelBridge) DisconnectWeChatKF() error {
	if b == nil || b.store == nil {
		return nil
	}
	config := b.store.View().WeChatKF
	err := b.store.ClearWeChatKF()
	if err == nil {
		b.auditRemoval(channelgateway.ChannelWeChatKF, config.CorpID)
	}
	return err
}

func (b *cloudChannelBridge) auditRemoval(channel channelgateway.Channel, account string) {
	if b.engine.channelGateway != nil {
		_ = b.engine.channelGateway.RecordAudit(context.Background(), channel, account, "configuration.remove", "", "")
	}
}

func (b *cloudChannelBridge) context() context.Context {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rootCtx != nil {
		return b.rootCtx
	}
	return context.Background()
}

func (b *cloudChannelBridge) handleSlackPayload(ctx context.Context, payload cloudchannels.SlackEventPayload) error {
	if b == nil || b.store == nil {
		return errors.New("Slack is unavailable")
	}
	config := b.store.View().Slack
	envelope, event, accepted, err := cloudchannels.NormalizeSlackEvent(config, payload)
	if err != nil || !accepted {
		return err
	}
	if err := b.materializeSlackMedia(ctx, config, &envelope); err != nil {
		return err
	}
	return b.accept(ctx, envelope, event.Channel, firstNonBlank(event.ThreadTS, event.TS))
}

func (b *cloudChannelBridge) handleDiscordMessage(ctx context.Context, message cloudchannels.DiscordMessage) error {
	if b == nil || b.store == nil {
		return errors.New("Discord is unavailable")
	}
	config := b.store.View().Discord
	envelope, accepted := cloudchannels.NormalizeDiscordMessage(config, message)
	if !accepted {
		return nil
	}
	if err := b.materializeDiscordMedia(ctx, &envelope); err != nil {
		return err
	}
	return b.accept(ctx, envelope, message.ChannelID, message.ID)
}

func (b *cloudChannelBridge) handleLarkPayload(ctx context.Context, payload cloudchannels.LarkEventPayload) error {
	config := b.store.View().Lark
	envelope, accepted, err := cloudchannels.NormalizeLarkMessage(config, payload)
	if err != nil || !accepted {
		return err
	}
	if err := b.materializeLarkMedia(ctx, config, &envelope); err != nil {
		return err
	}
	if b.handleApprovalCommand(ctx, envelope) {
		return nil
	}
	return b.accept(ctx, envelope, envelope.Address.ConversationID, envelope.ExternalMessageID)
}

func (b *cloudChannelBridge) handleLarkCardAction(ctx context.Context, payload cloudchannels.LarkCardActionPayload) (any, error) {
	config := b.store.View().Lark
	if payload.Header.AppID != config.AppID || payload.Header.Token != config.VerificationToken {
		return nil, errors.New("Lark card action identity is invalid")
	}
	action, _ := payload.Event.Action.Value["action"].(string)
	address := channelgateway.Address{Channel: channelgateway.ChannelLark, AccountID: payload.Header.TenantKey, ConversationID: payload.Event.Context.OpenChatID, ParticipantID: payload.Event.Operator.OpenID, Scope: channelgateway.ScopeGroup}
	if address.ConversationID == "" || address.ParticipantID == "" {
		return nil, errors.New("Lark card action address is invalid")
	}
	answered := b.answerApproval(ctx, address, action)
	envelope := channelgateway.Envelope{Direction: channelgateway.Inbound, Kind: channelgateway.KindApproval, Address: address, ExternalEventID: payload.Header.EventID, ExternalMessageID: payload.Event.Context.OpenMessageID, IdempotencyKey: "lark:action:" + payload.Header.EventID, Approval: &channelgateway.Approval{ID: payload.Header.EventID, Prompt: "Neo approval", Decision: action}, OccurredAt: time.Now().UTC()}
	if b.engine.channelGateway != nil {
		claim, claimErr := b.engine.channelGateway.ClaimInbound(ctx, envelope)
		if claimErr == nil && claim.State != channelgateway.ClaimDuplicate {
			_ = b.engine.channelGateway.CompleteInbound(ctx, envelope, "", "")
		}
	}
	if !answered {
		return map[string]any{"toast": map[string]string{"type": "warning", "content": "That approval is no longer active."}}, nil
	}
	message := "Denied. Neo is continuing."
	if action == "neo_gate_approve" {
		message = "Approved. Neo is continuing."
	}
	return map[string]any{"toast": map[string]string{"type": "success", "content": message}}, nil
}

func (b *cloudChannelBridge) handleDingTalkMessage(ctx context.Context, message cloudchannels.DingTalkMessage) error {
	config := b.store.View().DingTalk
	envelope, accepted, err := cloudchannels.NormalizeDingTalkMessage(config, message)
	if err != nil || !accepted {
		return err
	}
	if err := b.materializeDingTalkMedia(ctx, config, &envelope); err != nil {
		return err
	}
	if b.handleApprovalCommand(ctx, envelope) {
		return nil
	}
	return b.accept(ctx, envelope, envelope.Address.ConversationID, envelope.ExternalMessageID)
}

func (b *cloudChannelBridge) handleWeComBotMessage(ctx context.Context, message cloudchannels.WeComBotMessage) error {
	config := b.store.View().WeComBot
	envelope, accepted, err := cloudchannels.NormalizeWeComBotMessage(config, message)
	if err != nil || !accepted {
		return err
	}
	if err := b.materializeWeComBotMedia(ctx, config, &envelope); err != nil {
		return err
	}
	if b.handleApprovalCommand(ctx, envelope) {
		return nil
	}
	return b.accept(ctx, envelope, envelope.Address.ConversationID, envelope.ExternalMessageID)
}

func (b *cloudChannelBridge) handleWeComAppMessage(ctx context.Context, message cloudchannels.WeComAppMessage) error {
	config := b.store.View().WeComApp
	envelope, accepted := cloudchannels.NormalizeWeComAppMessage(config, message)
	if !accepted {
		return nil
	}
	if err := b.materializeWeComAppMedia(ctx, config, &envelope); err != nil {
		return err
	}
	if b.handleApprovalCommand(ctx, envelope) {
		return nil
	}
	return b.accept(ctx, envelope, envelope.Address.ConversationID, envelope.ExternalMessageID)
}

func (b *cloudChannelBridge) handleQQMessage(ctx context.Context, eventType string, message cloudchannels.QQMessage) error {
	config := b.store.View().QQ
	envelope, accepted := cloudchannels.NormalizeQQMessage(config, eventType, message)
	if !accepted {
		return nil
	}
	if err := b.materializeQQMedia(ctx, &envelope); err != nil {
		return err
	}
	if b.handleApprovalCommand(ctx, envelope) {
		return nil
	}
	return b.accept(ctx, envelope, envelope.Address.ConversationID, envelope.ExternalMessageID)
}

func (b *cloudChannelBridge) handleWeixinMessage(ctx context.Context, message cloudchannels.WeixinMessage) error {
	config := b.store.View().Weixin
	envelope, accepted := cloudchannels.NormalizeWeixinMessage(config, message)
	if !accepted {
		return nil
	}
	if token := strings.TrimSpace(message.ContextToken); token != "" {
		if err := b.store.UpdateWeixinProgress("", message.FromUserID, token, time.Now().Unix()); err != nil {
			return err
		}
	}
	if err := b.materializeWeixinMedia(ctx, config, &envelope); err != nil {
		return err
	}
	if b.handleApprovalCommand(ctx, envelope) {
		return nil
	}
	return b.accept(ctx, envelope, envelope.Address.ConversationID, envelope.ExternalMessageID)
}

func (b *cloudChannelBridge) handleWeChatMPMessage(ctx context.Context, message cloudchannels.WeChatMPMessage) error {
	config := b.store.View().WeChatMP
	envelope, accepted := cloudchannels.NormalizeWeChatMPMessage(config, message)
	if !accepted {
		return nil
	}
	if err := b.materializeWeChatMPMedia(ctx, config, &envelope); err != nil {
		return err
	}
	if b.handleApprovalCommand(ctx, envelope) {
		return nil
	}
	return b.accept(ctx, envelope, envelope.Address.ConversationID, envelope.ExternalMessageID)
}

func (b *cloudChannelBridge) consumeWeChatKF(ctx context.Context, eventToken, openKFID string) error {
	if eventToken == "" || openKFID == "" {
		return errors.New("WeChat Customer Service callback is incomplete")
	}
	b.mu.Lock()
	lock := b.kfLocks[openKFID]
	if lock == nil {
		lock = &sync.Mutex{}
		b.kfLocks[openKFID] = lock
	}
	b.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	for {
		config := b.store.View().WeChatKF
		cursor := config.Cursors[openKFID]
		page, err := b.wechatKF.Sync(ctx, config, eventToken, openKFID, cursor)
		if err != nil {
			return err
		}
		for _, message := range page.Messages {
			envelope, accepted := cloudchannels.NormalizeWeChatKFMessage(config, message)
			if !accepted {
				continue
			}
			if err := b.materializeWeChatKFMedia(ctx, config, &envelope); err != nil {
				return err
			}
			if b.handleApprovalCommand(ctx, envelope) {
				continue
			}
			if err := b.accept(ctx, envelope, openKFID, envelope.ExternalMessageID); err != nil {
				return err
			}
		}
		if page.NextCursor != "" {
			if err := b.store.UpdateWeChatKFCursor(openKFID, page.NextCursor); err != nil {
				return err
			}
		}
		if page.HasMore == 0 || page.NextCursor == "" || page.NextCursor == cursor {
			return nil
		}
	}
}

func (b *cloudChannelBridge) handleSlackAction(ctx context.Context, payload cloudchannels.SlackActionPayload) error {
	config := b.store.View().Slack
	if payload.Team.ID != config.TeamID || len(payload.Actions) != 1 {
		return errors.New("Slack action identity is invalid")
	}
	action := payload.Actions[0].ActionID
	conversation := payload.Channel.ID
	if payload.Message.ThreadTS != "" {
		conversation += ":" + payload.Message.ThreadTS
	}
	address := channelgateway.Address{Channel: channelgateway.ChannelSlack, AccountID: config.TeamID, ConversationID: conversation, ParticipantID: payload.User.ID, Scope: channelgateway.ScopeGroup}
	answered := b.answerApproval(ctx, address, action)
	if !answered && payload.Message.ThreadTS != "" {
		address.ConversationID = payload.Channel.ID
		answered = b.answerApproval(ctx, address, action)
	}
	eventID := firstNonBlank(payload.TriggerID, payload.Actions[0].ActionTS, payload.Message.TS+":"+action)
	envelope := channelgateway.Envelope{
		Direction: channelgateway.Inbound, Kind: channelgateway.KindApproval, Address: address,
		ExternalEventID: eventID, ExternalMessageID: payload.Message.TS, IdempotencyKey: "slack:action:" + eventID,
		Approval: &channelgateway.Approval{ID: eventID, Prompt: "Neo approval", Decision: action}, OccurredAt: time.Now().UTC(),
	}
	if b.engine.channelGateway != nil {
		claim, claimErr := b.engine.channelGateway.ClaimInbound(ctx, envelope)
		if claimErr == nil && claim.State != channelgateway.ClaimDuplicate {
			_ = b.engine.channelGateway.CompleteInbound(ctx, envelope, "", "")
		}
	}
	if !answered {
		return errors.New("Slack approval is no longer active")
	}
	return nil
}

func (b *cloudChannelBridge) accept(ctx context.Context, envelope channelgateway.Envelope, channelID, replyTo string) error {
	if envelope.Kind == channelgateway.KindInterrupt {
		if b.engine.channelGateway != nil {
			claim, err := b.engine.channelGateway.ClaimInbound(ctx, envelope)
			if err != nil {
				return err
			}
			if claim.State == channelgateway.ClaimDuplicate {
				return nil
			}
		}
		conversationID, ok := "", false
		if b.engine.channelGateway != nil {
			conversationID, ok, _ = b.engine.channelGateway.Resolve(ctx, envelope.Address)
		}
		if !ok {
			conversationID = channelConversationID(envelope.Address)
		}
		runID := b.engine.sessions.get(conversationID).interruptActive()
		text := "There is no active Neo task to stop."
		if runID != "" {
			text = "Stopping the current Neo task."
		}
		b.sendText(ctx, envelope, channelID, replyTo, "interrupt:"+envelope.IdempotencyKey, text)
		if b.engine.channelGateway != nil {
			_ = b.engine.channelGateway.CompleteInbound(ctx, envelope, conversationID, runID)
		}
		return nil
	}
	runID, conversationID, _, err := b.engine.acceptNormalizedMessage(ctx, envelope)
	if err != nil {
		return err
	}
	envelope.NeoConversation = conversationID
	b.sendProgress(ctx, envelope, channelID, replyTo)
	b.watchRun(runID, conversationID, envelope, channelID, replyTo)
	return nil
}

func (b *cloudChannelBridge) sendProgress(ctx context.Context, source channelgateway.Envelope, channelID, replyTo string) {
	if source.Address.Channel == channelgateway.ChannelWeComBot && b.state().WeComBot.Mode == "callback" {
		return
	}
	if b.engine.channelDispatch == nil {
		return
	}
	envelope := channelgateway.Envelope{
		Direction: channelgateway.Outbound, Kind: channelgateway.KindProgress,
		Address: source.Address, NeoConversation: source.NeoConversation,
		IdempotencyKey:  string(source.Address.Channel) + ":progress:" + source.IdempotencyKey,
		Progress:        &channelgateway.Progress{Stage: "working", Detail: "Neo is working on your request."},
		SideEffectClass: executortool.SideEffectNetwork, OccurredAt: time.Now().UTC(),
		Metadata: cloudDeliveryMetadata(source, channelID, replyTo),
	}
	if source.Address.Channel == channelgateway.ChannelLark {
		card, _ := json.Marshal(map[string]any{"config": map[string]bool{"wide_screen_mode": true}, "elements": []map[string]any{{"tag": "markdown", "content": "Neo is working on your request."}}})
		envelope.Metadata["lark_card"] = string(card)
	}
	_, _ = b.engine.channelDispatch.Dispatch(ctx, envelope, cloudOutboundSender{bridge: b, channel: source.Address.Channel})
}

func (b *cloudChannelBridge) drainLoop(ctx context.Context) {
	if b == nil || b.engine == nil || b.engine.channelDispatch == nil {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		b.drainOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *cloudChannelBridge) drainOnce(ctx context.Context) {
	state := b.state()
	if state.Slack.Configured() {
		_, _ = b.engine.channelDispatch.Drain(ctx, channelgateway.ChannelSlack, state.Slack.TeamID, cloudOutboundSender{bridge: b, channel: channelgateway.ChannelSlack}, 20)
	}
	if state.Discord.Configured() {
		_, _ = b.engine.channelDispatch.Drain(ctx, channelgateway.ChannelDiscord, state.Discord.ApplicationID, cloudOutboundSender{bridge: b, channel: channelgateway.ChannelDiscord}, 20)
	}
	if state.Lark.Configured() {
		_, _ = b.engine.channelDispatch.Drain(ctx, channelgateway.ChannelLark, state.Lark.AppID, cloudOutboundSender{bridge: b, channel: channelgateway.ChannelLark}, 20)
	}
	if state.DingTalk.Configured() {
		_, _ = b.engine.channelDispatch.Drain(ctx, channelgateway.ChannelDingTalk, state.DingTalk.ClientID, cloudOutboundSender{bridge: b, channel: channelgateway.ChannelDingTalk}, 20)
	}
	if state.WeComBot.Configured() {
		_, _ = b.engine.channelDispatch.Drain(ctx, channelgateway.ChannelWeComBot, state.WeComBot.BotID, cloudOutboundSender{bridge: b, channel: channelgateway.ChannelWeComBot}, 20)
	}
	if state.WeComApp.Configured() {
		_, _ = b.engine.channelDispatch.Drain(ctx, channelgateway.ChannelWeComApp, state.WeComApp.CorpID, cloudOutboundSender{bridge: b, channel: channelgateway.ChannelWeComApp}, 20)
	}
	if state.QQ.Configured() {
		_, _ = b.engine.channelDispatch.Drain(ctx, channelgateway.ChannelQQ, state.QQ.AppID, cloudOutboundSender{bridge: b, channel: channelgateway.ChannelQQ}, 20)
	}
	if state.Weixin.Configured() {
		_, _ = b.engine.channelDispatch.Drain(ctx, channelgateway.ChannelWeixin, state.Weixin.BotID, cloudOutboundSender{bridge: b, channel: channelgateway.ChannelWeixin}, 20)
	}
	if state.WeChatMP.Configured() {
		_, _ = b.engine.channelDispatch.Drain(ctx, channelgateway.ChannelWeChatMP, state.WeChatMP.AppID, cloudOutboundSender{bridge: b, channel: channelgateway.ChannelWeChatMP}, 20)
	}
	if state.WeChatKF.Configured() {
		_, _ = b.engine.channelDispatch.Drain(ctx, channelgateway.ChannelWeChatKF, state.WeChatKF.CorpID, cloudOutboundSender{bridge: b, channel: channelgateway.ChannelWeChatKF}, 20)
	}
}

func (b *cloudChannelBridge) watchRun(runID, conversationID string, source channelgateway.Envelope, channelID, replyTo string) {
	if runID == "" {
		return
	}
	b.mu.Lock()
	if b.watching[runID] {
		b.mu.Unlock()
		return
	}
	b.watching[runID] = true
	ctx := b.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	b.mu.Unlock()
	go func() {
		defer func() {
			b.mu.Lock()
			delete(b.watching, runID)
			b.mu.Unlock()
		}()
		replay, live, cancel := b.engine.broker.subscribe(runID, 0)
		defer cancel()
		for _, event := range replay {
			b.deliverEvent(ctx, source, channelID, replyTo, runID, event)
		}
		if live == nil {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-live:
				if !ok {
					return
				}
				b.deliverEvent(ctx, source, channelID, replyTo, runID, event)
			}
		}
	}()
}

func (b *cloudChannelBridge) deliverEvent(ctx context.Context, source channelgateway.Envelope, channelID, replyTo, runID string, event Event) {
	switch event.Type {
	case "chat.assistant":
		text, _ := event.Fields["text"].(string)
		for index, part := range splitCloudText(text, source.Address.Channel) {
			b.sendText(ctx, source, channelID, replyTo, fmt.Sprintf("%s:%d:text:%d", runID, event.Seq, index), part)
		}
	case "tool.media":
		ref, _ := event.Fields["url"].(string)
		if ref != "" {
			kind, _ := event.Fields["kind"].(string)
			b.sendMedia(ctx, source, channelID, replyTo, fmt.Sprintf("%s:%d:media", runID, event.Seq), ref, kind)
		}
	case "gate.invoked":
		nodeID, _ := event.Fields["node_id"].(string)
		question, _ := event.Fields["question"].(string)
		if nodeID == "" || strings.TrimSpace(question) == "" {
			return
		}
		b.mu.Lock()
		b.pending[cloudPendingKey(source.Address)] = cloudPending{runID: runID, nodeID: nodeID}
		b.mu.Unlock()
		if b.engine.channelGateway != nil {
			_ = b.engine.channelGateway.SetPending(ctx, channelgateway.PendingAction{
				Address: source.Address, Kind: channelgateway.KindApproval, RunID: runID, NodeID: nodeID, CreatedAt: time.Now().UTC(),
			})
		}
		b.sendApproval(ctx, source, channelID, replyTo, fmt.Sprintf("%s:%d:approval", runID, event.Seq), question)
	}
}

func (b *cloudChannelBridge) sendMedia(ctx context.Context, source channelgateway.Envelope, channelID, replyTo, key, ref, kind string) {
	if b.engine.channelDispatch == nil {
		return
	}
	mediaKind := channelgateway.MediaFile
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "image":
		mediaKind = channelgateway.MediaImage
	case "audio":
		mediaKind = channelgateway.MediaAudio
	case "video":
		mediaKind = channelgateway.MediaVideo
	}
	envelope := channelgateway.Envelope{
		Direction: channelgateway.Outbound, Kind: channelgateway.KindMessage,
		Address: source.Address, NeoConversation: source.NeoConversation,
		IdempotencyKey:  string(source.Address.Channel) + ":" + key,
		Media:           []channelgateway.Media{{Kind: mediaKind, Ref: ref}},
		SideEffectClass: executortool.SideEffectNetwork, OccurredAt: time.Now().UTC(),
		Metadata: cloudDeliveryMetadata(source, channelID, replyTo),
	}
	_, _ = b.engine.channelDispatch.Dispatch(ctx, envelope, cloudOutboundSender{bridge: b, channel: source.Address.Channel})
}

func (b *cloudChannelBridge) sendText(ctx context.Context, source channelgateway.Envelope, channelID, replyTo, key, text string) {
	if b.engine.channelDispatch == nil || strings.TrimSpace(text) == "" {
		return
	}
	envelope := channelgateway.Envelope{
		Direction: channelgateway.Outbound, Kind: channelgateway.KindMessage,
		Address: source.Address, NeoConversation: source.NeoConversation,
		IdempotencyKey: string(source.Address.Channel) + ":" + key, Text: text,
		SideEffectClass: executortool.SideEffectNetwork, OccurredAt: time.Now().UTC(),
		Metadata: cloudDeliveryMetadata(source, channelID, replyTo),
	}
	_, _ = b.engine.channelDispatch.Dispatch(ctx, envelope, cloudOutboundSender{bridge: b, channel: source.Address.Channel})
}

func (b *cloudChannelBridge) sendApproval(ctx context.Context, source channelgateway.Envelope, channelID, replyTo, key, prompt string) {
	if b.engine.channelDispatch == nil {
		return
	}
	envelope := channelgateway.Envelope{
		Direction: channelgateway.Outbound, Kind: channelgateway.KindApproval,
		Address: source.Address, NeoConversation: source.NeoConversation,
		IdempotencyKey: string(source.Address.Channel) + ":" + key, Text: prompt,
		Approval:        &channelgateway.Approval{ID: key, Prompt: prompt, Options: []string{"Approve", "Deny"}},
		SideEffectClass: executortool.SideEffectNetwork, OccurredAt: time.Now().UTC(),
		Metadata: cloudDeliveryMetadata(source, channelID, replyTo),
	}
	if source.Address.Channel == channelgateway.ChannelSlack {
		blocks := []map[string]any{
			{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": prompt}},
			{"type": "actions", "elements": []map[string]any{
				{"type": "button", "text": map[string]string{"type": "plain_text", "text": "Approve"}, "action_id": "neo_gate_approve", "style": "primary"},
				{"type": "button", "text": map[string]string{"type": "plain_text", "text": "Deny"}, "action_id": "neo_gate_deny", "style": "danger"},
			}},
		}
		encoded, _ := json.Marshal(blocks)
		envelope.Metadata["slack_blocks"] = string(encoded)
	} else if source.Address.Channel == channelgateway.ChannelDiscord {
		components := []map[string]any{{"type": 1, "components": []map[string]any{
			{"type": 2, "style": 3, "label": "Approve", "custom_id": "neo_gate_approve"},
			{"type": 2, "style": 4, "label": "Deny", "custom_id": "neo_gate_deny"},
		}}}
		encoded, _ := json.Marshal(components)
		envelope.Metadata["discord_components"] = string(encoded)
	} else if source.Address.Channel == channelgateway.ChannelLark {
		card := map[string]any{
			"config": map[string]bool{"wide_screen_mode": true},
			"elements": []map[string]any{
				{"tag": "markdown", "content": prompt},
				{"tag": "action", "actions": []map[string]any{
					{"tag": "button", "text": map[string]string{"tag": "plain_text", "content": "Approve"}, "type": "primary", "value": map[string]string{"action": "neo_gate_approve"}},
					{"tag": "button", "text": map[string]string{"tag": "plain_text", "content": "Deny"}, "type": "danger", "value": map[string]string{"action": "neo_gate_deny"}},
				}},
			},
		}
		encoded, _ := json.Marshal(card)
		envelope.Metadata["lark_card"] = string(encoded)
	} else {
		envelope.Text = prompt + "\n\nReply /approve or /deny."
	}
	_, _ = b.engine.channelDispatch.Dispatch(ctx, envelope, cloudOutboundSender{bridge: b, channel: source.Address.Channel})
}

func (b *cloudChannelBridge) answerApproval(ctx context.Context, address channelgateway.Address, action string) bool {
	if action != "neo_gate_approve" && action != "neo_gate_deny" {
		return false
	}
	key := cloudPendingKey(address)
	b.mu.Lock()
	pending, ok := b.pending[key]
	if ok {
		delete(b.pending, key)
	}
	b.mu.Unlock()
	if !ok && b.engine.channelGateway != nil {
		persisted, found, err := b.engine.channelGateway.Pending(ctx, address)
		if err == nil && found && persisted.Kind == channelgateway.KindApproval {
			pending = cloudPending{runID: persisted.RunID, nodeID: persisted.NodeID}
			ok = true
		}
	}
	if !ok {
		return false
	}
	run := b.engine.lookupRun(pending.runID)
	answered := run != nil && run.sess.answerGate(pending.nodeID, gateAnswer{approved: action == "neo_gate_approve"})
	if answered && b.engine.channelGateway != nil {
		_ = b.engine.channelGateway.ClearPending(ctx, address, pending.runID, pending.nodeID)
	}
	return answered
}

func (b *cloudChannelBridge) handleApprovalCommand(ctx context.Context, envelope channelgateway.Envelope) bool {
	command := strings.ToLower(strings.TrimSpace(envelope.Text))
	action := ""
	if command == "/approve" {
		action = "neo_gate_approve"
	} else if command == "/deny" {
		action = "neo_gate_deny"
	} else {
		return false
	}
	if b.engine.channelGateway != nil {
		claim, err := b.engine.channelGateway.ClaimInbound(ctx, envelope)
		if err != nil || claim.State == channelgateway.ClaimDuplicate {
			return true
		}
	}
	answered := b.answerApproval(ctx, envelope.Address, action)
	text := "That approval is no longer active."
	if answered && action == "neo_gate_approve" {
		text = "Approved. Neo is continuing."
	} else if answered {
		text = "Denied. Neo is continuing."
	}
	b.sendText(ctx, envelope, envelope.Address.ConversationID, envelope.ExternalMessageID, "approval-command:"+envelope.IdempotencyKey, text)
	if b.engine.channelGateway != nil {
		_ = b.engine.channelGateway.CompleteInbound(ctx, envelope, "", "")
	}
	return true
}

func cloudPendingKey(address channelgateway.Address) string {
	return strings.Join([]string{string(address.Channel), address.AccountID, address.ConversationID}, "\x00")
}

type cloudOutboundSender struct {
	bridge  *cloudChannelBridge
	channel channelgateway.Channel
}

func (s cloudOutboundSender) Send(ctx context.Context, envelope channelgateway.Envelope) (channelgateway.SendReceipt, error) {
	state := s.bridge.store.View()
	if len(envelope.Media) > 0 {
		name, data, err := readCloudRunMedia(s.bridge.engine, envelope.Media[0].Ref)
		if err != nil {
			return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "media", Message: err.Error(), Permanent: true}
		}
		switch s.channel {
		case channelgateway.ChannelSlack:
			return s.bridge.slackAPI.UploadFile(ctx, state.Slack.BotToken, envelope.Metadata["channel"], envelope.Metadata["thread_ts"], name, data, envelope.Text)
		case channelgateway.ChannelDiscord:
			return s.bridge.discordAPI.UploadFile(ctx, state.Discord.BotToken, envelope.Metadata["channel"], name, data, envelope.Text, envelope.Metadata["reply_to"])
		case channelgateway.ChannelLark:
			return s.bridge.larkAPI.PostMedia(ctx, state.Lark, envelope.Address.ConversationID, "chat_id", name, data, envelope.Media[0].Kind)
		case channelgateway.ChannelWeComApp:
			return s.bridge.wecomAPI.PostMedia(ctx, state.WeComApp, envelope.Address.ParticipantID, name, data, envelope.Media[0].Kind)
		case channelgateway.ChannelWeComBot:
			if state.WeComBot.Mode == "websocket" && s.bridge.wecomBot != nil {
				return s.bridge.wecomBot.SendMedia(ctx, envelope.Address.ConversationID, envelope.Address.Scope == channelgateway.ScopeGroup, name, data, envelope.Media[0].Kind)
			}
			return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "callback_media", Message: "WeCom Bot callback mode cannot deliver asynchronous binary media", Permanent: true}
		case channelgateway.ChannelQQ:
			return s.bridge.qqAPI.PostMedia(ctx, state.QQ, envelope.Address, envelope.Metadata["event_type"], envelope.Metadata["reply_to"], name, data, envelope.Media[0].Kind, qqDeliverySequence(envelope.IdempotencyKey))
		case channelgateway.ChannelWeixin:
			contextToken := state.Weixin.Contexts[envelope.Address.ParticipantID].Token
			return s.bridge.weixinAPI.SendMedia(ctx, state.Weixin, envelope.Address.ParticipantID, contextToken, name, data, envelope.Media[0].Kind)
		case channelgateway.ChannelWeChatMP:
			window, _ := strconv.ParseInt(envelope.Metadata["reply_window_started_at"], 10, 64)
			return s.bridge.wechatMP.PostMedia(ctx, state.WeChatMP, envelope.Address.ParticipantID, name, data, envelope.Media[0].Kind, window)
		case channelgateway.ChannelWeChatKF:
			window, _ := strconv.ParseInt(envelope.Metadata["reply_window_started_at"], 10, 64)
			return s.bridge.wechatKF.PostMedia(ctx, state.WeChatKF, envelope.Address.ParticipantID, envelope.Metadata["open_kfid"], name, data, envelope.Media[0].Kind, window)
		}
	}
	switch s.channel {
	case channelgateway.ChannelSlack:
		if envelope.Kind == channelgateway.KindProgress {
			return s.bridge.slackAPI.PostEphemeral(ctx, state.Slack.BotToken, envelope.Metadata["channel"], envelope.Metadata["user"], envelope.Metadata["thread_ts"], envelope.Progress.Detail)
		}
		var blocks any
		if encoded := envelope.Metadata["slack_blocks"]; encoded != "" {
			_ = json.Unmarshal([]byte(encoded), &blocks)
		}
		return s.bridge.slackAPI.PostMessage(ctx, state.Slack.BotToken, envelope.Metadata["channel"], envelope.Metadata["thread_ts"], envelope.Text, blocks)
	case channelgateway.ChannelDiscord:
		if envelope.Kind == channelgateway.KindProgress {
			return s.bridge.discordAPI.TriggerTyping(ctx, state.Discord.BotToken, envelope.Metadata["channel"])
		}
		var components any
		if encoded := envelope.Metadata["discord_components"]; encoded != "" {
			_ = json.Unmarshal([]byte(encoded), &components)
		}
		return s.bridge.discordAPI.PostMessage(ctx, state.Discord.BotToken, envelope.Metadata["channel"], envelope.Text, envelope.Metadata["reply_to"], components)
	case channelgateway.ChannelLark:
		if card := envelope.Metadata["lark_card"]; card != "" {
			return s.bridge.larkAPI.PostCard(ctx, state.Lark, envelope.Address.ConversationID, "chat_id", card)
		}
		return s.bridge.larkAPI.PostText(ctx, state.Lark, envelope.Address.ConversationID, "chat_id", envelope.Text)
	case channelgateway.ChannelDingTalk:
		return s.bridge.dingAPI.PostText(ctx, state.DingTalk, envelope.Address.ConversationID, envelope.Address.ParticipantID, envelope.Address.Scope == channelgateway.ScopeGroup, envelope.Text)
	case channelgateway.ChannelWeComBot:
		if state.WeComBot.Mode == "websocket" && s.bridge.wecomBot != nil {
			return s.bridge.wecomBot.SendText(ctx, envelope.Address.ConversationID, envelope.Address.Scope == channelgateway.ScopeGroup, envelope.Text)
		}
		return cloudchannels.PostWeComResponse(ctx, nil, envelope.Metadata["response_url"], envelope.Text)
	case channelgateway.ChannelWeComApp:
		return s.bridge.wecomAPI.PostText(ctx, state.WeComApp, envelope.Address.ParticipantID, envelope.Text)
	case channelgateway.ChannelQQ:
		return s.bridge.qqAPI.PostText(ctx, state.QQ, envelope.Address, envelope.Metadata["event_type"], envelope.Metadata["reply_to"], envelope.Text, qqDeliverySequence(envelope.IdempotencyKey))
	case channelgateway.ChannelWeixin:
		contextToken := state.Weixin.Contexts[envelope.Address.ParticipantID].Token
		var receipt channelgateway.SendReceipt
		var err error
		if envelope.Kind == channelgateway.KindProgress {
			receipt, err = s.bridge.weixinAPI.SendTyping(ctx, state.Weixin, envelope.Address.ParticipantID, contextToken, 1)
		} else {
			receipt, err = s.bridge.weixinAPI.SendText(ctx, state.Weixin, envelope.Address.ParticipantID, contextToken, envelope.Text)
		}
		var deliveryErr *channelgateway.DeliveryError
		if errors.As(err, &deliveryErr) && deliveryErr.Code == "weixin_-14" {
			_ = s.bridge.store.InvalidateWeixinContext(envelope.Address.ParticipantID)
		}
		return receipt, err
	case channelgateway.ChannelWeChatMP:
		window, _ := strconv.ParseInt(envelope.Metadata["reply_window_started_at"], 10, 64)
		return s.bridge.wechatMP.PostText(ctx, state.WeChatMP, envelope.Address.ParticipantID, envelope.Text, window)
	case channelgateway.ChannelWeChatKF:
		window, _ := strconv.ParseInt(envelope.Metadata["reply_window_started_at"], 10, 64)
		return s.bridge.wechatKF.PostText(ctx, state.WeChatKF, envelope.Address.ParticipantID, envelope.Metadata["open_kfid"], envelope.Text, window)
	default:
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "channel", Message: "unsupported cloud channel", Permanent: true}
	}
}

func qqDeliverySequence(key string) int { return int(crc32.ChecksumIEEE([]byte(key))%65535) + 1 }

func readCloudRunMedia(engine *Engine, ref string) (string, []byte, error) {
	if engine == nil {
		return "", nil, errors.New("media storage is unavailable")
	}
	bridge := &telegramBridge{engine: engine}
	return bridge.readRunMedia(ref)
}

func (b *cloudChannelBridge) materializeSlackMedia(ctx context.Context, config cloudchannels.SlackConfig, envelope *channelgateway.Envelope) error {
	for index := range envelope.Media {
		data, contentType, err := b.slackAPI.Download(ctx, config.BotToken, envelope.Media[index].Ref, uploadMaxBytes)
		if err != nil {
			return err
		}
		ref, err := b.storeInboundMedia(envelope.Media[index].Name, firstNonBlank(contentType, envelope.Media[index].MIMEType), data)
		if err != nil {
			return err
		}
		envelope.Media[index].Ref = ref
		envelope.Media[index].Size = int64(len(data))
	}
	return nil
}

func (b *cloudChannelBridge) materializeDiscordMedia(ctx context.Context, envelope *channelgateway.Envelope) error {
	for index := range envelope.Media {
		data, contentType, err := b.discordAPI.Download(ctx, envelope.Media[index].Ref, uploadMaxBytes)
		if err != nil {
			return err
		}
		ref, err := b.storeInboundMedia(envelope.Media[index].Name, firstNonBlank(contentType, envelope.Media[index].MIMEType), data)
		if err != nil {
			return err
		}
		envelope.Media[index].Ref = ref
		envelope.Media[index].Size = int64(len(data))
	}
	return nil
}

func (b *cloudChannelBridge) materializeLarkMedia(ctx context.Context, config cloudchannels.LarkConfig, envelope *channelgateway.Envelope) error {
	for index := range envelope.Media {
		parts := strings.SplitN(strings.TrimPrefix(envelope.Media[index].Ref, "lark-resource:"), ":", 2)
		if len(parts) != 2 {
			return errors.New("Lark media reference is invalid")
		}
		resourceType := "file"
		if envelope.Media[index].Kind == channelgateway.MediaImage {
			resourceType = "image"
		}
		name, data, err := b.larkAPI.DownloadResource(ctx, config, parts[0], parts[1], resourceType)
		if err != nil {
			return err
		}
		ref, err := b.storeInboundMedia(firstNonBlank(envelope.Media[index].Name, name), envelope.Media[index].MIMEType, data)
		if err != nil {
			return err
		}
		envelope.Media[index].Ref, envelope.Media[index].Name, envelope.Media[index].Size = ref, name, int64(len(data))
	}
	return nil
}

func (b *cloudChannelBridge) materializeWeComAppMedia(ctx context.Context, config cloudchannels.WeComAppConfig, envelope *channelgateway.Envelope) error {
	for index := range envelope.Media {
		mediaID := strings.TrimPrefix(envelope.Media[index].Ref, "wecom-app-media:")
		if mediaID == "" {
			return errors.New("WeCom media reference is invalid")
		}
		data, err := b.wecomAPI.DownloadMedia(ctx, config, mediaID)
		if err != nil {
			return err
		}
		ref, err := b.storeInboundMedia(envelope.Media[index].Name, envelope.Media[index].MIMEType, data)
		if err != nil {
			return err
		}
		envelope.Media[index].Ref, envelope.Media[index].Size = ref, int64(len(data))
	}
	return nil
}

func (b *cloudChannelBridge) materializeDingTalkMedia(ctx context.Context, config cloudchannels.DingTalkConfig, envelope *channelgateway.Envelope) error {
	for index := range envelope.Media {
		code := strings.TrimPrefix(envelope.Media[index].Ref, "dingtalk-resource:")
		if code == "" {
			return errors.New("DingTalk media reference is invalid")
		}
		data, contentType, err := b.dingAPI.Download(ctx, config, code, uploadMaxBytes)
		if err != nil {
			return err
		}
		ref, err := b.storeInboundMedia(envelope.Media[index].Name, firstNonBlank(contentType, envelope.Media[index].MIMEType), data)
		if err != nil {
			return err
		}
		envelope.Media[index].Ref, envelope.Media[index].Size = ref, int64(len(data))
	}
	return nil
}

func (b *cloudChannelBridge) materializeWeComBotMedia(ctx context.Context, config cloudchannels.WeComBotConfig, envelope *channelgateway.Envelope) error {
	for index := range envelope.Media {
		data, contentType, err := cloudchannels.DownloadWeComBotMedia(ctx, nil, envelope.Media[index].Ref, config.EncodingAESKey, uploadMaxBytes, "")
		if err != nil {
			return err
		}
		ref, err := b.storeInboundMedia(envelope.Media[index].Name, firstNonBlank(contentType, envelope.Media[index].MIMEType), data)
		if err != nil {
			return err
		}
		envelope.Media[index].Ref, envelope.Media[index].Size = ref, int64(len(data))
	}
	return nil
}

func (b *cloudChannelBridge) materializeQQMedia(ctx context.Context, envelope *channelgateway.Envelope) error {
	for index := range envelope.Media {
		data, contentType, err := b.qqAPI.Download(ctx, envelope.Media[index].Ref, uploadMaxBytes, "")
		if err != nil {
			return err
		}
		ref, err := b.storeInboundMedia(envelope.Media[index].Name, firstNonBlank(contentType, envelope.Media[index].MIMEType), data)
		if err != nil {
			return err
		}
		envelope.Media[index].Ref, envelope.Media[index].Size = ref, int64(len(data))
	}
	return nil
}
func (b *cloudChannelBridge) materializeWeixinMedia(ctx context.Context, config cloudchannels.WeixinConfig, envelope *channelgateway.Envelope) error {
	for index := range envelope.Media {
		data, err := b.weixinAPI.DownloadMedia(ctx, config, envelope.Media[index].Ref, uploadMaxBytes)
		if err != nil {
			return err
		}
		ref, err := b.storeInboundMedia(envelope.Media[index].Name, envelope.Media[index].MIMEType, data)
		if err != nil {
			return err
		}
		envelope.Media[index].Ref, envelope.Media[index].Size = ref, int64(len(data))
	}
	return nil
}
func (b *cloudChannelBridge) materializeWeChatMPMedia(ctx context.Context, config cloudchannels.WeChatMPConfig, envelope *channelgateway.Envelope) error {
	for index := range envelope.Media {
		mediaID := strings.TrimPrefix(envelope.Media[index].Ref, "wechat-mp-media:")
		if mediaID == "" {
			return errors.New("WeChat Official Account media reference is invalid")
		}
		data, contentType, err := b.wechatMP.DownloadMedia(ctx, config, mediaID, uploadMaxBytes)
		if err != nil {
			return err
		}
		ref, err := b.storeInboundMedia(envelope.Media[index].Name, contentType, data)
		if err != nil {
			return err
		}
		envelope.Media[index].Ref, envelope.Media[index].Size = ref, int64(len(data))
	}
	return nil
}
func (b *cloudChannelBridge) materializeWeChatKFMedia(ctx context.Context, config cloudchannels.WeChatKFConfig, envelope *channelgateway.Envelope) error {
	for index := range envelope.Media {
		mediaID := strings.TrimPrefix(envelope.Media[index].Ref, "wechat-kf-media:")
		if mediaID == "" {
			return errors.New("WeChat Customer Service media reference is invalid")
		}
		data, contentType, err := b.wechatKF.DownloadMedia(ctx, config, mediaID, uploadMaxBytes)
		if err != nil {
			return err
		}
		ref, err := b.storeInboundMedia(envelope.Media[index].Name, contentType, data)
		if err != nil {
			return err
		}
		envelope.Media[index].Ref, envelope.Media[index].Size = ref, int64(len(data))
	}
	return nil
}

func (b *cloudChannelBridge) storeInboundMedia(name, mimeType string, data []byte) (string, error) {
	if b.engine.mediaDir == "" {
		return "", errors.New("media storage is not configured on this Neo daemon")
	}
	if len(data) > uploadMaxBytes {
		return "", fmt.Errorf("channel media is larger than the %d MB upload limit", uploadMaxBytes>>20)
	}
	if err := os.MkdirAll(b.engine.mediaDir, 0o700); err != nil {
		return "", err
	}
	ext := extForUpload(name, mimeType)
	fileName := mintMediaID() + ext
	file, err := os.OpenFile(filepath.Join(b.engine.mediaDir, fileName), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	var writeErr error
	if b.engine.mediaSealEnabled() {
		_, writeErr = sealMediaCopy(b.engine, fileName, file, bytes.NewReader(data))
	} else {
		_, writeErr = io.Copy(file, bytes.NewReader(data))
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(filepath.Join(b.engine.mediaDir, fileName))
		if writeErr != nil {
			return "", writeErr
		}
		return "", closeErr
	}
	return "/media/" + fileName, nil
}

func splitCloudText(text string, channel channelgateway.Channel) []string {
	limit := 3500
	if channel == channelgateway.ChannelDiscord {
		limit = 1900
	} else if channel == channelgateway.ChannelWeComApp || channel == channelgateway.ChannelWeComBot || channel == channelgateway.ChannelQQ || channel == channelgateway.ChannelWeixin || channel == channelgateway.ChannelWeChatMP || channel == channelgateway.ChannelWeChatKF {
		limit = 1900
	}
	return splitTelegramText(text, limit)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func cloudDeliveryMetadata(source channelgateway.Envelope, channelID, replyTo string) map[string]string {
	metadata := map[string]string{"channel": channelID, "reply_to": replyTo, "thread_ts": replyTo, "user": source.Address.ParticipantID}
	for _, key := range []string{"response_url", "event_type", "context_token", "open_kfid", "reply_window_started_at"} {
		if value := source.Metadata[key]; value != "" {
			metadata[key] = value
		}
	}
	return metadata
}

func safeCloudError(message string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	if index := strings.Index(message, "?ticket="); index >= 0 {
		message = message[:index] + "?ticket=[redacted]"
	}
	return message
}
