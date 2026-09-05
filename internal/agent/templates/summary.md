CRITICAL: Respond with text only. Do not call tools.

Create a precise, detailed continuation summary of the conversation. Preserve the information needed to continue the work without rereading earlier messages.

Before the final summary, use an <analysis> block as a private drafting area to check chronology, completeness, and technical accuracy. Then put the final handoff in a <summary> block. The drafting block will be removed before the summary is saved.

While preparing the summary:

1. Analyze the conversation chronologically.
2. Identify every explicit user request and every correction or change of direction.
3. Treat the newest explicit user direction as authoritative when it conflicts with older work.
4. Do not reactivate completed, superseded, or interrupted tasks as the current objective.
5. Distinguish completed work, unfinished work, hypotheses, and directly verified evidence.
6. Preserve exact file paths, symbols, commands, errors, validation results, and important code snippets when they are needed to continue.
7. Pay special attention to the most recent user and assistant messages and the exact point where work stopped.

The <summary> block must contain these sections:

1. Primary Request and Intent
   - State the user's current request and intended observable outcome.
   - Include direct quotes from the newest messages that establish the current task.

2. Key Technical Concepts
   - List the important technologies, components, APIs, and constraints.

3. Files and Code Sections
   - List files examined, changed, or still needing work.
   - Explain why each file matters and include essential code details.

4. Errors and Fixes
   - Record errors, failed approaches, user corrections, and how they were addressed.

5. Problem Solving and Evidence
   - Separate confirmed findings from unresolved hypotheses.
   - Record the evidence that supports each important conclusion.

6. All User Messages
   - List every user message that is not a tool result.
   - Preserve wording when it communicates feedback, urgency, or changed intent.

7. Completed Work
   - State what is actually complete and how it was verified.

8. Pending Tasks
   - List only unfinished tasks that still follow from the newest user direction.
   - Mark older tasks as superseded instead of presenting them as current work.

9. Current Work and Exact Cutoff
   - Describe precisely what was being done immediately before compaction.
   - Identify the last completed action and the next uncompleted action.

10. Next Step
    - Give the single next action that directly continues the newest request.
    - Include direct quotes from the latest conversation that prevent intent drift.

Return exactly one <analysis> block followed by exactly one <summary> block. Do not call tools. Do not add text outside those blocks.
