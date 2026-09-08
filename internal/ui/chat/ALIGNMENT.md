# Chat alignment contract

Tool renderers receive their usable item width from `baseToolMessageItem.RawRender`. That boundary reserves the actual message prefix and applies the readability cap once. Renderers must not call `cappedMessageWidth` again or impose another local cap. Existing full-width edit tools retain their container-level policy.

`Styles.Tool.Body` owns the outer body inset. Use `toolBodyWidth(styles, itemWidth)` to derive panel width, and apply `Tool.Body.Render` exactly once. Do not duplicate its padding as a numeric constant.

Panel-level helpers (`renderSummaryPanel`, `toolOutputPlainContent`, `toolOutputCodePanel`, `toolOutputMarkdownPanel`) accept panel width and do not add the outer body inset. Item-level helpers (`toolOutputCodeContent`, `toolOutputMarkdownContent`, diff helpers, and `renderToolResultTextContent`) accept item width and add the body inset themselves. `renderSummaryCard` accepts panel width and adds the outer body inset.

Summary and Markdown text use the shared SummaryPanel inner padding. Code has an intentional line-number gutter; its complete frame, including padding, belongs inside the same panel width. Panel backgrounds share their left and right edges regardless of content format.

Disclosure controls use `renderOutputFooter`: a content-width blue tab attached directly to the panel's bottom-left edge. Do not add another body wrapper, left padding, or blank separator around the tab.

Skip blank and whitespace-only thinking lines, including empty rendered Markdown rows. A single nonblank logical line stays fully visible even when it wraps, with no expansion control or toggle action. Only multiple nonblank lines are collapsible. All-blank thinking renders no branch or thinking click target.

Thinking branches derive their indentation from the same Tool.Body left inset. Thoughts following tool output join the preceding item without a blank row; turn-opening thoughts keep their floating branch and normal inter-message gap. List rendering, height, scrolling, selection, and mouse coordinates use `GapAfter` for this conditional spacing.

`alignment_test.go` verifies complete message renderings at compact, normal, and capped widths, with both default and changed body insets. Extend its fixtures when adding an output renderer. Keep intentional content gutters distinct from outer panel alignment.
