package server

import (
	"net/url"
	"testing"

	"github.com/example-git/crux/internal/message"
	"github.com/stretchr/testify/require"
)

func pageIDs(messages []message.Message) []string {
	ids := make([]string, len(messages))
	for index, item := range messages {
		ids[index] = item.ID
	}
	return ids
}

func TestMessagePagePreservesFullListingWithoutParameters(t *testing.T) {
	messages := []message.Message{{ID: "a"}, {ID: "b"}}
	page, err := parseMessagePage(url.Values{})
	require.NoError(t, err)
	result, more, err := page.apply(messages)
	require.NoError(t, err)
	require.False(t, page.paged())
	require.False(t, more)
	require.Equal(t, []string{"a", "b"}, pageIDs(result))
}

func TestMessagePageReturnsNewestAndOlderPages(t *testing.T) {
	messages := []message.Message{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}, {ID: "e"}}
	page, err := parseMessagePage(url.Values{"limit": {"2"}})
	require.NoError(t, err)
	result, more, err := page.apply(messages)
	require.NoError(t, err)
	require.True(t, more)
	require.Equal(t, []string{"d", "e"}, pageIDs(result))

	page, err = parseMessagePage(url.Values{"limit": {"2"}, "before": {"d"}})
	require.NoError(t, err)
	result, more, err = page.apply(messages)
	require.NoError(t, err)
	require.True(t, more)
	require.Equal(t, []string{"b", "c"}, pageIDs(result))

	page, err = parseMessagePage(url.Values{"limit": {"5"}, "before": {"b"}})
	require.NoError(t, err)
	result, more, err = page.apply(messages)
	require.NoError(t, err)
	require.False(t, more)
	require.Equal(t, []string{"a"}, pageIDs(result))
}

func TestMessagePageRejectsInvalidParameters(t *testing.T) {
	for _, query := range []url.Values{{"limit": {"0"}}, {"limit": {"1001"}}, {"limit": {"x"}}, {"before": {"a"}}} {
		_, err := parseMessagePage(query)
		require.Error(t, err, query)
	}
	page, err := parseMessagePage(url.Values{"limit": {"2"}, "before": {"missing"}})
	require.NoError(t, err)
	_, _, err = page.apply([]message.Message{{ID: "a"}})
	require.ErrorContains(t, err, "before")
}
