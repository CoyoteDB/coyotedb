// What to run on the next computer when this node runs in a container
// (D68): the same image with its setup page, or the command line.
import type { DockerJoin, Role } from "../api/types";
import { CodeBlock } from "./ui";

export function DockerJoinSteps({ d, role }: { d: DockerJoin; role: Role }) {
  const witness = role === "witness";
  return (
    <div class="stack" style={{ "--gap": "12px" }}>
      <div>
        <span class="label">Easiest: its setup page</span>
        <p class="ink2" style={{ margin: "6px 0 10px" }}>
          On the other computer, start CoyoteDB in Docker
          {witness ? " (a witness needs no port 5432)" : ""}:
        </p>
        <CodeBlock
          title="Terminal"
          code={d.setup_command}
          what="docker command"
        >
          <span class="prompt">$ </span>
          {d.setup_command}
        </CodeBlock>
        <p class="ink2" style={{ margin: "10px 0 0" }}>
          Then <code>{d.link_command}</code> shows a sign-in link. Open it in a
          browser on that computer, choose <strong>Join</strong>, paste the code
          {witness ? (
            <>
              {" "}
              and pick <strong>A witness</strong>
            </>
          ) : null}
          .
        </p>
      </div>
      <div>
        <span class="label">Or the command line</span>
        <p class="ink2" style={{ margin: "6px 0 10px" }}>
          Replace{" "}
          {witness ? (
            ""
          ) : (
            <>
              <code>NAME</code> with a name for that computer and{" "}
            </>
          )}
          <code>ADDRESS</code> with its LAN or Tailscale address:
        </p>
        <CodeBlock title="Terminal" code={d.cli_command} what="docker command">
          <span class="prompt">$ </span>
          {d.cli_command}
        </CodeBlock>
      </div>
    </div>
  );
}
