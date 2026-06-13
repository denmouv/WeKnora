package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// AgentFileCache bridges Agent tool execution and IM reply post-processing.
// Tools store file bytes keyed by refID; the IM service loads them when
// constructing the outgoing ReplyMessage.
var AgentFileCache = &sync.Map{}

var downloadDocumentTool = BaseTool{
	name: ToolDownloadDocument,
	description: `Download the original file (e.g., PDF) for a knowledge-base document.

## When to Use
Call this tool when the user asks to receive the paper or document PDF.

## Parameters
- knowledge_id (required): The document's knowledge ID from search results.

## Returns
A file marker like [FILE:abc12345:Title.pdf 3.5MB] that the system
automatically resolves to the actual file for delivery to the user.

## Notes
- Call once per document the user wants to download.
- The file is delivered in the same message as the text reply.`,
	schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "knowledge_id": {
      "type": "string",
      "description": "The document knowledge ID to download"
    }
  },
  "required": ["knowledge_id"]
}`),
}

// DownloadDocumentInput defines the input for the download_document tool.
type DownloadDocumentInput struct {
	KnowledgeID string `json:"knowledge_id"`
}

// DownloadDocumentTool retrieves the original file for a knowledge entry and
// caches it for the IM reply pipeline.
type DownloadDocumentTool struct {
	BaseTool
	knowledgeService interfaces.KnowledgeService
	fileService      interfaces.FileService
}

// NewDownloadDocumentTool creates a new download document tool.
func NewDownloadDocumentTool(
	knowledgeService interfaces.KnowledgeService,
	fileService interfaces.FileService,
) *DownloadDocumentTool {
	return &DownloadDocumentTool{
		BaseTool:         downloadDocumentTool,
		knowledgeService: knowledgeService,
		fileService:      fileService,
	}
}

// Execute reads the document file from storage, caches it in AgentFileCache,
// and returns a [FILE:refID:name size] marker for the IM service to resolve.
func (t *DownloadDocumentTool) Execute(
	ctx context.Context, args json.RawMessage,
) (*types.ToolResult, error) {
	var input DownloadDocumentInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("invalid arguments: %v", err),
		}, nil
	}

	if input.KnowledgeID == "" {
		return &types.ToolResult{
			Success: false,
			Error:   "knowledge_id is required",
		}, nil
	}

	knowledge, err := t.knowledgeService.GetKnowledgeByIDOnly(ctx, input.KnowledgeID)
	if err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("document not found: %v", err),
		}, nil
	}

	filePath := knowledge.FilePath
	if filePath == "" {
		return &types.ToolResult{
			Success: false,
			Error:   "document has no stored file",
		}, nil
	}

	reader, err := t.fileService.GetFile(ctx, filePath)
	if err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("read file: %v", err),
		}, nil
	}
	defer reader.Close()

	fileData, err := io.ReadAll(reader)
	if err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("read content: %v", err),
		}, nil
	}

	title := strings.TrimSpace(knowledge.Title)
	if title == "" {
		title = strings.TrimSpace(knowledge.FileName)
	}
	if title == "" {
		title = input.KnowledgeID
	}

	// Cache for the IM reply pipeline.  LoadAndDelete in extractFilesFromAnswer
	// ensures single-use semantics — each marker is consumed exactly once.
	refID := fmt.Sprintf("file_%s_%d",
		input.KnowledgeID[:min(8, len(input.KnowledgeID))], len(fileData))
	AgentFileCache.Store(refID, fileData)

	safeTitle := strings.NewReplacer(
		"]", "", "[", "", "\n", " ", "\r", "",
	).Replace(title)
	if strings.HasSuffix(strings.ToLower(safeTitle), ".pdf") {
		safeTitle = safeTitle[:len(safeTitle)-4]
	}
	sizeMB := float64(len(fileData)) / (1024 * 1024)
	marker := fmt.Sprintf("[FILE:%s:%s.pdf %.1fMB]", refID, safeTitle, sizeMB)

	return &types.ToolResult{
		Success: true,
		Output:  marker,
		Data: map[string]interface{}{
			"knowledge_id": input.KnowledgeID,
			"title":        title,
			"size_bytes":   len(fileData),
			"file_ref":     refID,
		},
	}, nil
}
