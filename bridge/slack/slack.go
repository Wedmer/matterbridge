package bslack

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/42wim/matterbridge/bridge"
	"github.com/42wim/matterbridge/bridge/config"
	"github.com/42wim/matterbridge/bridge/helper"
	"github.com/42wim/matterbridge/matterhook"
	"github.com/hashicorp/golang-lru"
	"github.com/nlopes/slack"
	"github.com/rs/xid"
)

type Bslack struct {
	sync.RWMutex
	*bridge.Config

	mh  *matterhook.Client
	sc  *slack.Client
	rtm *slack.RTM
	si  *slack.Info

	cache        *lru.Cache
	uuid         string
	useChannelID bool

	users      map[string]*slack.User
	usersMutex sync.RWMutex

	channelsByID   map[string]*slack.Channel
	channelsByName map[string]*slack.Channel
	channelsMutex  sync.RWMutex
}

const (
	sChannelJoin     = "channel_join"
	sChannelLeave    = "channel_leave"
	sMessageDeleted  = "message_deleted"
	sSlackAttachment = "slack_attachment"
	sPinnedItem      = "pinned_item"
	sUnpinnedItem    = "unpinned_item"
	sChannelTopic    = "channel_topic"
	sChannelPurpose  = "channel_purpose"
	sFileComment     = "file_comment"
	sMeMessage       = "me_message"
	sUserTyping      = "user_typing"
	sLatencyReport   = "latency_report"
	sSystemUser      = "system"

	tokenConfig           = "Token"
	incomingWebhookConfig = "WebhookBindAddress"
	outgoingWebhookConfig = "WebhookURL"
	skipTLSConfig         = "SkipTLSVerify"
	useNickPrefixConfig   = "PrefixMessagesWithNick"
	editDisableConfig     = "EditDisable"
	editSuffixConfig      = "EditSuffix"
	iconURLConfig         = "iconurl"
	noSendJoinConfig      = "nosendjoinpart"
)

func New(cfg *bridge.Config) bridge.Bridger {
	newCache, err := lru.New(5000)
	if err != nil {
		cfg.Log.Fatalf("Could not create LRU cache for Slack bridge: %v", err)
	}
	b := &Bslack{
		Config:         cfg,
		uuid:           xid.New().String(),
		cache:          newCache,
		users:          map[string]*slack.User{},
		channelsByID:   map[string]*slack.Channel{},
		channelsByName: map[string]*slack.Channel{},
	}
	return b
}

func (b *Bslack) Command(cmd string) string {
	return ""
}

func (b *Bslack) Connect() error {
	b.RLock()
	defer b.RUnlock()
	if b.GetString(incomingWebhookConfig) != "" {
		if b.GetString(outgoingWebhookConfig) != "" {
			b.Log.Info("Connecting using webhookurl (sending) and webhookbindaddress (receiving)")
			b.mh = matterhook.New(b.GetString(outgoingWebhookConfig), matterhook.Config{
				InsecureSkipVerify: b.GetBool(skipTLSConfig),
				BindAddress:        b.GetString(incomingWebhookConfig),
			})
		} else if b.GetString(tokenConfig) != "" {
			b.Log.Info("Connecting using token (sending)")
			b.sc = slack.New(b.GetString(tokenConfig))
			b.rtm = b.sc.NewRTM()
			go b.rtm.ManageConnection()
			b.Log.Info("Connecting using webhookbindaddress (receiving)")
			b.mh = matterhook.New(b.GetString(outgoingWebhookConfig), matterhook.Config{
				InsecureSkipVerify: b.GetBool(skipTLSConfig),
				BindAddress:        b.GetString(incomingWebhookConfig),
			})
		} else {
			b.Log.Info("Connecting using webhookbindaddress (receiving)")
			b.mh = matterhook.New(b.GetString(outgoingWebhookConfig), matterhook.Config{
				InsecureSkipVerify: b.GetBool(skipTLSConfig),
				BindAddress:        b.GetString(incomingWebhookConfig),
			})
		}
		go b.handleSlack()
		return nil
	}
	if b.GetString(outgoingWebhookConfig) != "" {
		b.Log.Info("Connecting using webhookurl (sending)")
		b.mh = matterhook.New(b.GetString(outgoingWebhookConfig), matterhook.Config{
			InsecureSkipVerify: b.GetBool(skipTLSConfig),
			DisableServer:      true,
		})
		if b.GetString(tokenConfig) != "" {
			b.Log.Info("Connecting using token (receiving)")
			b.sc = slack.New(b.GetString(tokenConfig))
			b.rtm = b.sc.NewRTM()
			go b.rtm.ManageConnection()
			go b.handleSlack()
		}
	} else if b.GetString(tokenConfig) != "" {
		b.Log.Info("Connecting using token (sending and receiving)")
		b.sc = slack.New(b.GetString(tokenConfig))
		b.rtm = b.sc.NewRTM()
		go b.rtm.ManageConnection()
		go b.handleSlack()
	}
	if b.GetString(incomingWebhookConfig) == "" && b.GetString(outgoingWebhookConfig) == "" && b.GetString(tokenConfig) == "" {
		return errors.New("no connection method found. See that you have WebhookBindAddress, WebhookURL or Token configured")
	}
	return nil
}

func (b *Bslack) Disconnect() error {
	return b.rtm.Disconnect()
}

// JoinChannel only acts as a verification method that checks whether Matterbridge's
// Slack integration is already member of the channel. This is because Slack does not
// allow apps or bots to join channels themselves and they need to be invited
// manually by a user.
func (b *Bslack) JoinChannel(channel config.ChannelInfo) error {
	b.populateChannels()

	channelInfo, err := b.getChannel(channel.Name)
	if err != nil {
		return fmt.Errorf("could not join channel: %#v", err)
	}

	if strings.HasPrefix(channel.Name, "ID:") {
		b.useChannelID = true
		channel.Name = channelInfo.Name
	}

	if !channelInfo.IsMember {
		return fmt.Errorf("slack integration that matterbridge is using is not member of channel '%s', please add it manually", channelInfo.Name)
	}
	return nil
}

func (b *Bslack) Reload(cfg *bridge.Config) (string, error) {
	return "", nil
}

func (b *Bslack) Send(msg config.Message) (string, error) {
	b.Log.Debugf("=> Receiving %#v", msg)

	// Make a action /me of the message
	if msg.Event == config.EVENT_USER_ACTION {
		msg.Text = "_" + msg.Text + "_"
	}

	// Use webhook to send the message
	if b.GetString(outgoingWebhookConfig) != "" {
		return b.sendWebhook(msg)
	}
	return b.sendRTM(msg)
}

// sendWebhook uses the configured WebhookURL to send the message
func (b *Bslack) sendWebhook(msg config.Message) (string, error) {
	// skip events
	if msg.Event != "" {
		return "", nil
	}

	if b.GetBool(useNickPrefixConfig) {
		msg.Text = msg.Username + msg.Text
	}

	if msg.Extra != nil {
		// this sends a message only if we received a config.EVENT_FILE_FAILURE_SIZE
		for _, rmsg := range helper.HandleExtra(&msg, b.General) {
			iconURL := config.GetIconURL(&rmsg, b.GetString(iconURLConfig))
			matterMessage := matterhook.OMessage{
				IconURL:  iconURL,
				Channel:  msg.Channel,
				UserName: rmsg.Username,
				Text:     rmsg.Text,
			}
			if err := b.mh.Send(matterMessage); err != nil {
				b.Log.Errorf("Failed to send message: %v", err)
			}
		}

		// webhook doesn't support file uploads, so we add the url manually
		for _, f := range msg.Extra["file"] {
			fi := f.(config.FileInfo)
			if fi.URL != "" {
				msg.Text += " " + fi.URL
			}
		}
	}

	// if we have native slack_attachments add them
	var attachs []slack.Attachment
	for _, attach := range msg.Extra[sSlackAttachment] {
		attachs = append(attachs, attach.([]slack.Attachment)...)
	}

	iconURL := config.GetIconURL(&msg, b.GetString(iconURLConfig))
	matterMessage := matterhook.OMessage{
		IconURL:     iconURL,
		Attachments: attachs,
		Channel:     msg.Channel,
		UserName:    msg.Username,
		Text:        msg.Text,
	}
	if msg.Avatar != "" {
		matterMessage.IconURL = msg.Avatar
	}
	err := b.mh.Send(matterMessage)
	if err != nil {
		b.Log.Error(err)
		return "", err
	}
	return "", nil
}

func (b *Bslack) sendRTM(msg config.Message) (string, error) {
	channelInfo, err := b.getChannel(msg.Channel)
	if err != nil {
		return "", fmt.Errorf("could not send message: %v", err)
	}

	// Delete message
	if msg.Event == config.EVENT_MSG_DELETE {
		// some protocols echo deletes, but with empty ID
		if msg.ID == "" {
			return "", nil
		}
		// we get a "slack <ID>", split it
		ts := strings.Fields(msg.ID)
		_, _, err = b.rtm.DeleteMessage(channelInfo.ID, ts[1])
		if err != nil {
			return msg.ID, err
		}
		return msg.ID, nil
	}

	// Prepend nick if configured
	if b.GetBool(useNickPrefixConfig) {
		msg.Text = msg.Username + msg.Text
	}

	// Edit message if we have an ID
	if msg.ID != "" {
		ts := strings.Fields(msg.ID)
		_, _, _, err = b.rtm.UpdateMessage(channelInfo.ID, ts[1], msg.Text)
		if err != nil {
			return msg.ID, err
		}
		return msg.ID, nil
	}

	messageParameters := b.prepareMessageParameters(&msg)

	// Upload a file if it exists
	if msg.Extra != nil {
		for _, rmsg := range helper.HandleExtra(&msg, b.General) {
			_, _, err = b.rtm.PostMessage(channelInfo.ID, rmsg.Username+rmsg.Text, *messageParameters)
			if err != nil {
				b.Log.Error(err)
			}
		}
		// Upload files if necessary (from Slack, Telegram or Mattermost).
		b.handleUploadFile(&msg, channelInfo.ID)
	}

	// Post normal message
	_, id, err := b.rtm.PostMessage(channelInfo.ID, msg.Text, *messageParameters)
	if err != nil {
		return "", err
	}
	return "slack " + id, nil
}

func (b *Bslack) prepareMessageParameters(msg *config.Message) *slack.PostMessageParameters {
	params := slack.NewPostMessageParameters()
	if b.GetBool(useNickPrefixConfig) {
		params.AsUser = true
	}
	params.Username = msg.Username
	params.LinkNames = 1 // replace mentions
	params.IconURL = config.GetIconURL(msg, b.GetString(iconURLConfig))
	if msg.Avatar != "" {
		params.IconURL = msg.Avatar
	}
	// add a callback ID so we can see we created it
	params.Attachments = append(params.Attachments, slack.Attachment{CallbackID: "matterbridge_" + b.uuid})
	// add file attachments
	params.Attachments = append(params.Attachments, b.createAttach(msg.Extra)...)
	// add slack attachments (from another slack bridge)
	if msg.Extra != nil {
		for _, attach := range msg.Extra[sSlackAttachment] {
			params.Attachments = append(params.Attachments, attach.([]slack.Attachment)...)
		}
	}
	return &params
}

func (b *Bslack) createAttach(extra map[string][]interface{}) []slack.Attachment {
	var attachements []slack.Attachment
	for _, v := range extra["attachments"] {
		entry := v.(map[string]interface{})
		s := slack.Attachment{
			Fallback:   extractStringField(entry, "fallback"),
			Color:      extractStringField(entry, "color"),
			Pretext:    extractStringField(entry, "pretext"),
			AuthorName: extractStringField(entry, "author_name"),
			AuthorLink: extractStringField(entry, "author_link"),
			AuthorIcon: extractStringField(entry, "author_icon"),
			Title:      extractStringField(entry, "title"),
			TitleLink:  extractStringField(entry, "title_link"),
			Text:       extractStringField(entry, "text"),
			ImageURL:   extractStringField(entry, "image_url"),
			ThumbURL:   extractStringField(entry, "thumb_url"),
			Footer:     extractStringField(entry, "footer"),
			FooterIcon: extractStringField(entry, "footer_icon"),
		}
		attachements = append(attachements, s)
	}
	return attachements
}

func extractStringField(data map[string]interface{}, field string) string {
	if rawValue, found := data[field]; found {
		if value, ok := rawValue.(string); ok {
			return value
		}
	}
	return ""
}
