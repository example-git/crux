package server

import (
	"errors"
	"net/url"
	"strconv"

	"github.com/example-git/crux/internal/message"
)

const maxMessagePageLimit = 1000

type messagePage struct {
	limit  int
	before string
}

func parseMessagePage(query url.Values) (messagePage, error) {
	page := messagePage{before: query.Get("before")}
	if value := query.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > maxMessagePageLimit {
			return page, errors.New("limit must be a whole number from 1 to 1000")
		}
		page.limit = limit
	} else if page.before != "" {
		return page, errors.New("before requires limit")
	}
	return page, nil
}

func (p messagePage) paged() bool {
	return p.limit > 0
}

func (p messagePage) apply(messages []message.Message) ([]message.Message, bool, error) {
	if !p.paged() {
		return messages, false, nil
	}
	end := len(messages)
	if p.before != "" {
		end = -1
		for index, item := range messages {
			if item.ID == p.before {
				end = index
				break
			}
		}
		if end < 0 {
			return nil, false, errors.New("before does not identify a message in this session")
		}
	}
	start := max(0, end-p.limit)
	return messages[start:end], start > 0, nil
}
