package proto

// MCPResource is cached metadata from one workspace's connected MCP server.
type MCPResource struct {
	MCPName  string `json:"mcp_name"`
	URI      string `json:"uri"`
	Name     string `json:"name"`
	MIMEType string `json:"mime_type,omitempty"`
}
