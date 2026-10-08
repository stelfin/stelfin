package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stelfin/stelfin/chat"
	"github.com/stelfin/stelfin/chat/chattest"
)

// handlerFunc adapts a function to Handler, so each case below can say in one
// line what the handler did.
type handlerFunc func(ctx context.Context, m chat.Inbound, out Replier, links Linker) error

func (f handlerFunc) Handle(ctx context.Context, m chat.Inbound, out Replier, links Linker) error {
	return f(ctx, m, out, links)
}

// inboundServer is the least of a Server that handleInbound touches: a
// handler, somewhere for replies to go, and a log.
func inboundServer(t *testing.T, h Handler) (*Server, *chattest.Fake) {
	t.Helper()
	transports, fake := testTransports(t)
	return &Server{
		cfg: ServerConfig{Handler: h, Transports: transports},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, fake
}

func command(name string) chat.Inbound {
	m := chattest.Message("telegram:1", "42", "")
	m.Command = name
	return m
}

func interaction(name string) chat.Inbound {
	m := command(name)
	m.Conversation.InteractionToken = "token"
	return m
}

// TestAPanickingHandlerDoesNotTakeTheServerDown: the work runs in a goroutine
// nothing else recovers, so an unrecovered panic there ends the process and
// every other conversation with it. This test completing is the assertion; the
// reply is what the person who triggered it is owed.
func TestAPanickingHandlerDoesNotTakeTheServerDown(t *testing.T) {
	srv, fake := inboundServer(t, handlerFunc(
		func(context.Context, chat.Inbound, Replier, Linker) error { panic("nil map") }))

	srv.handleInbound(t.Context(), chat.Telegram, command("pay"))

	sent := fake.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d replies after a panic, want 1", len(sent))
	}
	if got := sent[0].Reply.Text; got != inboundFailedReply {
		t.Errorf("reply = %q, want the failure notice", got)
	}
	if strings.Contains(sent[0].Reply.Text, "nil map") {
		t.Error("the panic value reached the person who sent the message")
	}
	if !sent[0].Reply.Ephemeral {
		t.Error("the failure notice was not private to the sender")
	}
}

// TestAFailureThatSaidNothingIsReported: an error that reaches only the log
// looks, from the chat, exactly like a bot that is not there.
func TestAFailureThatSaidNothingIsReported(t *testing.T) {
	srv, fake := inboundServer(t, handlerFunc(
		func(context.Context, chat.Inbound, Replier, Linker) error {
			return errors.New("store: connection refused to 10.0.0.7:5432")
		}))

	srv.handleInbound(t.Context(), chat.Telegram, command("whoami"))

	sent := fake.Sent()
	if len(sent) != 1 || sent[0].Reply.Text != inboundFailedReply {
		t.Fatalf("sent = %+v, want one failure notice", sent)
	}
	if strings.Contains(sent[0].Reply.Text, "10.0.0.7") {
		t.Error("the error's contents reached the person who sent the message")
	}
}

// TestAHandlerThatAnsweredIsNotAnsweredOver: the handler's own words stand,
// whether or not it then returned an error, and whether or not they arrived.
func TestAHandlerThatAnsweredIsNotAnsweredOver(t *testing.T) {
	for name, deliver := range map[string]error{
		"delivered":   nil,
		"undelivered": errors.New("telegram: 403 bot was blocked by the user"),
	} {
		t.Run(name, func(t *testing.T) {
			srv, fake := inboundServer(t, handlerFunc(
				func(ctx context.Context, m chat.Inbound, out Replier, _ Linker) error {
					_ = out.Send(ctx, m.Conversation, m.Actor, chat.Reply{Text: "its own answer", Ephemeral: true})
					return errors.New("and then it failed")
				}))
			fake.SetSendErr(deliver)

			srv.handleInbound(t.Context(), chat.Telegram, command("pay"))

			// One attempt, not two: a second message would be a second try at
			// reaching somebody the platform will not let us reach.
			if deliver == nil {
				if sent := fake.Sent(); len(sent) != 1 || sent[0].Reply.Text != "its own answer" {
					t.Fatalf("sent = %+v, want only the handler's reply", sent)
				}
			} else if sent := fake.Sent(); len(sent) != 0 {
				t.Fatalf("sent = %+v, want nothing more after an undeliverable reply", sent)
			}
		})
	}
}

// TestSilenceIsKeptForGroupChatter: free text in a group may not have been
// meant for the bot at all. A failure reading it is logged and nobody is told.
func TestSilenceIsKeptForGroupChatter(t *testing.T) {
	failing := handlerFunc(func(context.Context, chat.Inbound, Replier, Linker) error {
		return errors.New("decoder: timeout")
	})
	quiet := handlerFunc(func(context.Context, chat.Inbound, Replier, Linker) error { return nil })

	for name, h := range map[string]Handler{"failed": failing, "declined": quiet} {
		t.Run(name, func(t *testing.T) {
			srv, fake := inboundServer(t, h)
			m := chattest.Message("telegram:2", "42", "lunch at one?")
			m.Conversation.IsDM = false
			srv.handleInbound(t.Context(), chat.Telegram, m)
			if sent := fake.Sent(); len(sent) != 0 {
				t.Fatalf("sent = %+v, want silence", sent)
			}
		})
	}
}

// TestAnInteractionIsAlwaysClosed: Discord has already been told an answer is
// coming and shows a spinner until one does. A handler that declines to answer
// is right to for a message in a group, and leaves an interaction hanging.
func TestAnInteractionIsAlwaysClosed(t *testing.T) {
	srv, fake := inboundServer(t, handlerFunc(
		func(context.Context, chat.Inbound, Replier, Linker) error { return nil }))

	srv.handleInbound(t.Context(), chat.Telegram, interaction("whoami"))

	sent := fake.Sent()
	if len(sent) != 1 || sent[0].Reply.Text != inboundNothingReply {
		t.Fatalf("sent = %+v, want the interaction closed", sent)
	}
}

// TestADirectMessageThatFailsIsAnswered: nobody else is in a direct message, so
// whatever was typed there was meant for the bot.
func TestADirectMessageThatFailsIsAnswered(t *testing.T) {
	srv, fake := inboundServer(t, handlerFunc(
		func(context.Context, chat.Inbound, Replier, Linker) error { return errors.New("boom") }))

	m := chattest.Message("telegram:3", "42", "hello")
	m.Conversation.IsDM = true
	srv.handleInbound(t.Context(), chat.Telegram, m)

	if sent := fake.Sent(); len(sent) != 1 || sent[0].Reply.Text != inboundFailedReply {
		t.Fatalf("sent = %+v, want one failure notice", sent)
	}
}
