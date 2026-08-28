package handler

import (
	"context"

	"github.com/17xande-dev/gostore/internal/orders"
	"github.com/17xande-dev/mailer"
)

// Mail for a paid order, and the one invariant that governs all of it: **the order
// is already recorded paid before any of this runs.** A mail server that is down,
// slow, or misconfigured must never be able to lose a sale, so nothing here can
// fail the payment callback and nothing here is retried by failing it.
//
// Two messages go out, and they are deliberately separate sends rather than one
// message with two recipients: the customer's copy is a receipt and the owner's is
// a work order, they say different things, and one of them failing should not
// suppress the other.
//
//   - The customer gets a confirmation, once. orders.emailed records that, so a
//     replayed gateway notification does not send a second receipt.
//   - Whoever packs the parcel gets a notification, if ORDER_NOTIFY_EMAIL is set.
//     It also carries the oversell warning, which otherwise only exists in the
//     logs — the person who has to tell a customer their item is not in stock
//     after all should not have to find that in a log aggregator.

// orderMailData is what both order emails render from.
type orderMailData struct {
	StoreName string
	Currency  string
	BaseURL   string
	Order     orders.Order

	// Oversold names the lines whose stock could not be decremented. Only the
	// owner's copy shows it; telling a customer their order may not be
	// deliverable, in the same breath as confirming it, is not the way to find
	// out.
	Oversold []string

	// Downloads are the links this payment created, one per digital line. They
	// appear only in the customer's copy: the owner has no use for somebody else's
	// download link, and putting a working credential in a second inbox is the
	// kind of thing that is obvious once it has happened.
	//
	// This is the only moment these exist in readable form — only the hash is
	// stored — so an email that fails to send costs the buyer their link, and the
	// admin has to issue a new entitlement. That is the deliberate cost of a
	// database dump not being a set of working links.
	Downloads []DownloadLink
}

// DownloadLink is one buyer's link, ready to render.
type DownloadLink struct {
	Title string
	Label string
	URL   string
}

// sendOrderEmails delivers the receipt and the notification for a paid order. It
// returns nothing: every outcome here is logged and none of them changes what the
// caller does.
func (h *Handler) sendOrderEmails(ctx context.Context, order orders.Order, oversold []string, grants []orders.Grant) {
	data := orderMailData{
		StoreName: h.cfg.StoreName,
		Currency:  h.cfg.Currency,
		BaseURL:   h.cfg.BaseURL,
		Order:     order,
		Oversold:  oversold,
		Downloads: h.downloadLinks(grants),
	}
	log := h.log.With("order", order.ID)

	if order.Emailed {
		// A replay, or a retry after the notification email failed. Either way the
		// customer already has their receipt.
		log.Info("confirmation already sent; not sending another")
	} else if err := h.sendConfirmation(ctx, data); err != nil {
		// Logged and dropped. The order stands, and the customer has already seen
		// a confirmation page with their reference on it.
		log.Error("failed to send the order confirmation", "to", order.Customer.Email, "error", err)
	} else if err := h.orders.MarkEmailed(ctx, order.ID); err != nil {
		// The mail went out but the flag did not stick, so a retry would send a
		// second copy. Worth logging loudly and not worth failing over.
		log.Error("sent the confirmation but failed to record it", "error", err)
	} else {
		log.Info("sent the order confirmation", "to", order.Customer.Email)
	}

	if h.cfg.OrderNotifyEmail == "" {
		return
	}
	if err := h.sendOwnerNotification(ctx, data); err != nil {
		log.Error("failed to notify the store owner", "to", h.cfg.OrderNotifyEmail, "error", err)
		return
	}
	log.Info("notified the store owner", "to", h.cfg.OrderNotifyEmail)
}

// downloadLinks turns freshly minted grants into absolute URLs.
//
// Absolute, and from BaseURL rather than from the request: this renders into an
// email, where a relative href points at the reader's mail client and nothing at
// all.
func (h *Handler) downloadLinks(grants []orders.Grant) []DownloadLink {
	if len(grants) == 0 {
		return nil
	}
	out := make([]DownloadLink, 0, len(grants))
	for _, g := range grants {
		out = append(out, DownloadLink{
			Title: g.Title,
			Label: g.VariantLabel,
			URL:   h.cfg.BaseURL + "/downloads/" + g.Token,
		})
	}
	return out
}

func (h *Handler) sendConfirmation(ctx context.Context, data orderMailData) error {
	text, err := h.tmpl.Text("email_order_paid.txt", data)
	if err != nil {
		return err
	}
	html, err := h.tmpl.String("email_order_paid", data)
	if err != nil {
		return err
	}
	return h.mail.Send(ctx, mailer.Message{
		To:      []string{data.Order.Customer.Email},
		Subject: data.StoreName + " order " + data.Order.Reference() + " — payment received",
		Text:    text,
		HTML:    html,
	})
}

func (h *Handler) sendOwnerNotification(ctx context.Context, data orderMailData) error {
	text, err := h.tmpl.Text("email_order_notify.txt", data)
	if err != nil {
		return err
	}
	subject := "New order " + data.Order.Reference() + " — " + data.Currency + " " +
		formatCents(data.Order.TotalCents)
	if len(data.Oversold) > 0 {
		// In the subject, because it is the one thing on this page that needs
		// acting on before the parcel is packed.
		subject = "OVERSOLD: " + subject
	}
	return h.mail.Send(ctx, mailer.Message{
		To:      []string{h.cfg.OrderNotifyEmail},
		Subject: subject,
		Text:    text,
	})
}
