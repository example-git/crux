package chat

import (
	"fmt"
	"strings"

	"github.com/example-git/crux/internal/diffdetect"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
)

func looksLikeDiff(content string) bool {
	return diffdetect.IsUnifiedDiff(content)
}

func toolOutputDiffContentFromUnified(sty *styles.Styles, content string, width int, expanded bool) string {
	bodyWidth := toolBodyWidth(sty, width)
	formatter := common.DiffFormatter(sty).Patch(common.StripCursorControl(content)).Wrap(expanded).Width(bodyWidth)
	if width > maxTextWidth {
		formatter.Split()
	}
	formatted := formatter.String()
	lines := strings.Split(formatted, "\n")
	if len(lines) > responseContextHeight && !expanded {
		truncation := sty.Tool.DiffTruncation.Width(bodyWidth).Render(fmt.Sprintf(assistantMessageTruncateFormat, len(lines)-responseContextHeight))
		formatted = strings.Join(lines[:responseContextHeight], "\n") + "\n" + truncation
	}
	return sty.Tool.Body.Render(formatted)
}
