{{.Instructions}}

<env>
Working directory: {{.WorkingDir}}
Is directory a git repo: {{if .IsGitRepo}} yes {{else}} no {{end}}
Platform: {{.Platform}}
{{if .RenderDate}}Today's date: {{.Date}}
{{end}}</env>
