package messaging

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"controlplane/protocol"

	"github.com/nats-io/nats.go"
)

const (
	defaultURL      = nats.DefaultURL
	defaultUsername = "application"
	defaultPassword = "controlplane-app-local"
)
const operationTimeout = 5 * time.Second

type Client struct {
	connection *nats.Conn
	jetstream  nats.JetStreamContext
}

type registrationResult struct {
	Error string `json:"error,omitempty"`
}

type registrationRequest struct {
	Kind         protocol.ResourceKind `json:"kind"`
	ReplySubject string                `json:"replySubject"`
}

func Connect(url, name string) (*Client, error) {
	username := os.Getenv("NATS_USERNAME")
	if username == "" {
		username = os.Getenv("NATS_APP_USERNAME")
	}
	if username == "" {
		username = defaultUsername
	}
	password := os.Getenv("NATS_PASSWORD")
	if password == "" {
		password = defaultPassword
	}
	connection, err := nats.Connect(url, nats.Name(name), nats.UserInfo(username, password))
	if err != nil {
		return nil, err
	}

	jetstream, err := connection.JetStream()
	if err != nil {
		connection.Close()
		return nil, err
	}

	return &Client{connection: connection, jetstream: jetstream}, nil
}

func URLFromEnv() string {
	if url := os.Getenv("NATS_URL"); url != "" {
		return url
	}
	return defaultURL
}

func (c *Client) Close() {
	c.connection.Close()
}

func (c *Client) EnsureRegistrationStream() error {
	return c.ensureStream(RegistrationStreamName, RegistrationSubject)
}

func (c *Client) EnsureKindEventStream(apiVersion, kind string) error {
	return c.ensureStream(KindEventStreamName(apiVersion, kind), KindEventSubject(apiVersion, kind))
}

func (c *Client) ensureStream(name, subject string) error {
	info, err := c.jetstream.StreamInfo(name)
	if err == nil {
		if !hasExactSubject(info.Config.Subjects, subject) {
			return fmt.Errorf("JetStream stream %q has unexpected subjects %q", name, info.Config.Subjects)
		}
		return nil
	}

	if existingName, lookupErr := c.jetstream.StreamNameBySubject(subject); lookupErr == nil {
		existingInfo, infoErr := c.jetstream.StreamInfo(existingName)
		if infoErr != nil {
			return infoErr
		}
		if !hasExactSubject(existingInfo.Config.Subjects, subject) {
			return fmt.Errorf("JetStream stream %q matches subject %q through a wildcard", existingName, subject)
		}
		return nil
	}

	_, err = c.jetstream.AddStream(&nats.StreamConfig{
		Name:      name,
		Subjects:  []string{subject},
		Retention: nats.LimitsPolicy,
		Storage:   nats.FileStorage,
		Discard:   nats.DiscardOld,
		MaxAge:    7 * 24 * time.Hour,
	})
	if err == nil {
		return nil
	}

	info, infoErr := c.jetstream.StreamInfo(name)
	if infoErr == nil && hasExactSubject(info.Config.Subjects, subject) {
		return nil
	}
	return err
}

func hasExactSubject(subjects []string, subject string) bool {
	for _, configuredSubject := range subjects {
		if configuredSubject == subject {
			return true
		}
	}
	return false
}

func (c *Client) RegisterKind(ctx context.Context, kind protocol.ResourceKind) error {
	if err := c.EnsureRegistrationStream(); err != nil {
		return err
	}

	replySubject := c.connection.NewInbox()
	replySubscription, err := c.connection.SubscribeSync(replySubject)
	if err != nil {
		return err
	}
	defer replySubscription.Unsubscribe()
	if err := replySubscription.AutoUnsubscribe(1); err != nil {
		return err
	}

	body, err := json.Marshal(registrationRequest{Kind: kind, ReplySubject: replySubject})
	if err != nil {
		return err
	}
	operationContext, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if _, err := c.jetstream.PublishMsg(&nats.Msg{
		Subject: RegistrationSubject,
		Data:    body,
	}, nats.Context(operationContext)); err != nil {
		return err
	}

	response, err := replySubscription.NextMsgWithContext(ctx)
	if err != nil {
		return err
	}
	var result registrationResult
	if err := json.Unmarshal(response.Data, &result); err != nil {
		return fmt.Errorf("decode kind registration response: %w", err)
	}
	if result.Error != "" {
		return errors.New(result.Error)
	}
	return nil
}

func (c *Client) RunRegistrations(
	ctx context.Context,
	handler func(context.Context, protocol.ResourceKind) error,
) error {
	if err := c.EnsureRegistrationStream(); err != nil {
		return err
	}
	subscription, err := c.jetstream.PullSubscribe(
		RegistrationSubject,
		RegistrationConsumer,
		nats.BindStream(RegistrationStreamName),
		nats.ManualAck(),
	)
	if err != nil {
		return err
	}
	defer subscription.Unsubscribe()

	for ctx.Err() == nil {
		messages, err := subscription.Fetch(1, nats.MaxWait(time.Second))
		if errors.Is(err, nats.ErrTimeout) {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		for _, message := range messages {
			var request registrationRequest
			if err := json.Unmarshal(message.Data, &request); err != nil {
				if err := message.Term(); err != nil {
					return err
				}
				continue
			}

			handlerErr := handler(ctx, request.Kind)
			result := registrationResult{}
			if handlerErr != nil {
				result.Error = handlerErr.Error()
			}
			if err := c.replyRegistration(ctx, request.ReplySubject, result); err != nil {
				return err
			}
			if err := acknowledge(ctx, message); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (c *Client) replyRegistration(ctx context.Context, replySubject string, result registrationResult) error {
	if replySubject == "" {
		return nil
	}
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if err := c.connection.Publish(replySubject, body); err != nil {
		return err
	}
	operationContext, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	return c.connection.FlushWithContext(operationContext)
}

func (c *Client) PublishEvent(ctx context.Context, event protocol.WatchEvent) error {
	subject := KindEventSubject(event.Object.APIVersion, event.Object.Kind)
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	operationContext, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	_, err = c.jetstream.PublishMsg(&nats.Msg{Subject: subject, Data: body}, nats.Context(operationContext))
	return err
}

func (c *Client) RunKindEvents(
	ctx context.Context,
	apiVersion, kind, durable string,
	handler func(protocol.WatchEvent) error,
) error {
	subject := KindEventSubject(apiVersion, kind)
	stream, err := c.jetstream.StreamNameBySubject(subject)
	if err != nil {
		return err
	}
	info, err := c.jetstream.StreamInfo(stream)
	if err != nil {
		return err
	}
	if !hasExactSubject(info.Config.Subjects, subject) {
		return fmt.Errorf("JetStream stream %q does not own event subject %q", stream, subject)
	}
	subscription, err := c.jetstream.PullSubscribe(
		subject,
		durable,
		nats.BindStream(stream),
		nats.ManualAck(),
	)
	if err != nil {
		return err
	}
	defer subscription.Unsubscribe()

	for ctx.Err() == nil {
		messages, err := subscription.Fetch(1, nats.MaxWait(time.Second))
		if errors.Is(err, nats.ErrTimeout) {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		for _, message := range messages {
			var event protocol.WatchEvent
			if err := json.Unmarshal(message.Data, &event); err != nil {
				if err := message.Term(); err != nil {
					return err
				}
				continue
			}
			if event.Object.APIVersion != apiVersion || event.Object.Kind != kind {
				if err := acknowledge(ctx, message); err != nil {
					return err
				}
				continue
			}
			if err := handler(event); err != nil {
				if nakErr := message.NakWithDelay(2 * time.Second); nakErr != nil {
					return nakErr
				}
				continue
			}
			if err := acknowledge(ctx, message); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func acknowledge(ctx context.Context, message *nats.Msg) error {
	operationContext, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	return message.AckSync(nats.Context(operationContext))
}
func StableDurableName(controller, apiVersion, kind string) string {
	identity := append(encodeKind(apiVersion, kind), []byte(controller)...)
	hash := sha256.Sum256(identity)
	return controller + "-" + fmt.Sprintf("%x", hash[:8])
}
