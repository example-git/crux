Analyze this codebase and create/update **{{.Config.Options.InitializeAs}}** to help future sessions work effectively in this repository.

**First**: Check if directory is empty or contains only config files. If so, stop and say "Directory appears empty or only contains config. Add source code first, then run this command to generate {{.Config.Options.InitializeAs}}."

**Goal**: Document what a tool-use session needs to work in this codebase — commands, patterns, conventions, gotchas, overall architecture, how components fit together

**Discovery process**:

1. Check directory contents with `ls`
2. Look for existing rule files (`.cursor/rules/*.md`, `.cursorrules`, `.github/copilot-instructions.md`, `claude.md`, `agents.md`) - only read if they exist
3. Identify project type from config files and directory structure
4. Find build/test/lint commands from config files, scripts, Makefiles, or CI configs
5. Read representative source files to capture code patterns, architecture, control/data flow
6. If {{.Config.Options.InitializeAs}} exists, read and improve it

**Content to include**:

- Essential commands (build, test, run, deploy, etc.) - whatever is relevant for this project
- Code organization and structure, application architecture and control/data flow
- Naming conventions and style patterns
- Testing approach and patterns
- Important gotchas or non-obvious patterns
- Any project-specific context from existing rule files

**Note:** Token engines absorb context from files they read, so mentioning obvious details visible in any single file is actively detrimental. Focus on non-obvious knowledge that prevents trial-and-error discovery: gotchas, implicit conventions, commands with surprising flags, and context that isn't self-evident from the code.

**Format**: Clear markdown sections. Structure based on what the codebase contains. Aim for completeness over brevity — include everything a future session would need.

**Critical**: Only document what you actually observe. Never invent commands, patterns, or conventions. If you can't find something, don't include it.
