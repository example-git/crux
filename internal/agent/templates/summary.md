CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.

- Do NOT use Read, Bash, search, Edit, MultiEdit, Write, or ANY other tool.
- You already have all the context you need in the conversation above.
- Tool calls will be REJECTED and will waste your only turn — you will fail the task.
- Your entire response must be plain text: an <analysis> block followed by a <summary> block.

Create a precise, detailed continuation summary of the conversation so far. Preserve the information needed to continue the work without rereading earlier messages.

Before the final summary, use an <analysis> block as a private drafting area to check chronology, completeness, and technical accuracy. Then put the final handoff in a <summary> block. The drafting block will be removed before the summary is saved.

Be concise and relevant, not exhaustive. If the conversation is long and covers multiple old, completed, or superseded topics, do not produce a full play-by-play of every topic. Give old, resolved topics only a brief mention (what it was, that it is done) and spend the bulk of the detail — full code snippets, exact file paths, verbatim quotes — on the most recent task and whatever is still unfinished or directly needed to continue it.

While preparing the summary:

1. Analyze the conversation chronologically. For each section identify the user's explicit requests and intents, the approach taken, key decisions, technical concepts, and code patterns (file names, full code snippets, function signatures, and file edits where relevant).
2. Identify every explicit user request and every correction or change of direction.
3. Treat the newest explicit user direction as authoritative when it conflicts with older work.
4. Do not reactivate completed, superseded, or interrupted tasks as the current objective.
5. Distinguish completed work, unfinished work, hypotheses, and directly verified evidence.
6. Preserve exact file paths, symbols, commands, errors, validation results, and important code snippets when they are needed to continue.
7. Pay special attention to the most recent user and assistant messages and the exact point where work stopped.
8. Keep old, closed-out topics compressed to a short recap; do not spend length re-narrating work that is already finished and not relevant to the current task.
9. Double-check the draft for technical accuracy and completeness before writing the final summary.

The <summary> block must contain these sections:

1. Primary Request and Intent
   - Capture all of the user's explicit requests and intents in detail.
   - Include direct quotes from the newest messages that establish the current task.

2. Key Technical Concepts
   - List all important technical concepts, technologies, and frameworks discussed.

3. Files and Code Sections
   - Enumerate specific files and code sections examined, modified, or created that still matter to the current or unfinished task.
   - Explain why each file matters and include full code snippets where applicable.
   - For files touched only by old, fully completed topics, name them briefly without re-deriving their full diff/history.

4. Errors and Fixes
   - List all errors encountered and how they were fixed.
   - Pay special attention to specific user feedback received, especially where the user asked for something to be done differently.

5. Problem Solving and Evidence
   - Document problems solved and any ongoing troubleshooting.
   - Separate confirmed findings from unresolved hypotheses, and record the evidence supporting each important conclusion.

6. All User Messages
   - List user messages that are not tool results, preserving wording when it communicates feedback, urgency, or changed intent.
   - For a long conversation with many old, resolved topics, it is enough to briefly group or paraphrase messages belonging to those closed topics; give verbatim, individually listed treatment to messages from the current/most recent topic.

7. Completed Work
   - State what is actually complete and how it was verified.

8. Pending Tasks
   - Outline any pending tasks explicitly requested that still follow from the newest user direction.
   - Mark older tasks as superseded instead of presenting them as current work.

9. Current Work and Exact Cutoff
   - Describe in detail precisely what was being worked on immediately before this summary request, paying special attention to the most recent messages from both user and assistant.
   - Identify the last completed action and the next uncompleted action, including file names and code snippets where applicable.

10. Next Step
    - List the single next step that is directly in line with the user's most recent explicit requests and the task being worked on immediately before this summary request.
    - Do not start on tangential or old, already-completed requests without confirming with the user first.
    - Include direct quotes from the most recent conversation showing exactly what task was being worked on and where it left off, verbatim, so there is no drift in task interpretation.

Here is an example of how your output should be structured:

<example>
<analysis>
[Your thought process, ensuring all points above are covered thoroughly and accurately]
</analysis>

<summary>
1. Primary Request and Intent:
   [Detailed description]

2. Key Technical Concepts:
   - [Concept 1]
   - [Concept 2]
   - [...]

3. Files and Code Sections:
   - [File Name 1]
      - [Summary of why this file is important]
      - [Summary of the changes made to this file, if any]
      - [Important Code Snippet]
   - [File Name 2]
      - [Important Code Snippet]
   - [...]

4. Errors and Fixes:
    - [Detailed description of error 1]:
      - [How it was fixed]
      - [User feedback on the error, if any]
    - [...]

5. Problem Solving and Evidence:
   [Description of solved problems, confirmed findings vs. open hypotheses, and ongoing troubleshooting]

6. All User Messages:
    - [Detailed non-tool-result user message]
    - [...]

7. Completed Work:
   [What is actually complete and how it was verified]

8. Pending Tasks:
   - [Task 1]
   - [Task 2]
   - [...]

9. Current Work and Exact Cutoff:
   [Precise description of current work, and the exact point where it stopped]

10. Next Step:
   [Next step to take, with direct quotes from the latest messages]

</summary>
</example>

There may be additional summarization instructions provided below under "Additional Instructions". If present, follow them when creating the summary above; if absent, disregard this paragraph.

Return exactly one <analysis> block followed by exactly one <summary> block. Do not call tools. Do not add text outside those blocks.
