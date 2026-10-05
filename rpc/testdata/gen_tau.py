"""Record tau's RPC mode responding to fixed command scripts.

malachi's rpc golden test replays the same scripts against its own server and
compares the structure of what comes back. Regenerate with:

    cd ~/src/tau && uv run python ~/src/malachi/rpc/testdata/gen_tau.py

Each scenario writes <name>.cmds.jsonl (the input, with {"__wait": seconds}
pauses) and tau_<name>.jsonl (tau's output, one record per line).
"""

from __future__ import annotations

import json
import os
import sys
import tempfile
import threading
import time
from pathlib import Path

import anyio

TAU = Path(__file__).resolve()
sys.path.insert(0, str(Path.home() / "src/tau/tests"))

from pi_event_helpers import assistant_done, assistant_start, text_delta  # noqa: E402
from tau_agent import AssistantMessage, Usage, UserMessage  # noqa: E402
from tau_agent.session import JsonlSessionStorage, MessageEntry  # noqa: E402
from tau_ai import FakeProvider  # noqa: E402
from tau_coding import CodingSession, CodingSessionConfig  # noqa: E402
from tau_coding.rpc import RpcServer  # noqa: E402

OUT = Path(__file__).resolve().parent


def reply(text: str, usage: Usage | None = None) -> list:
    msg = AssistantMessage(content=text, model="fake", usage=usage or Usage())
    return [assistant_start(model="fake"), text_delta(text), assistant_done(msg)]


class SlowProvider(FakeProvider):
    """A FakeProvider whose first stream waits, so commands arrive mid-run."""

    def __init__(self, streams, delay: float) -> None:
        super().__init__(streams)
        self._delay = delay
        self._first = True

    def stream_response(self, **kwargs):
        inner = super().stream_response(**kwargs)
        delay = self._delay if self._first else 0
        self._first = False

        async def iterator():
            await anyio.sleep(delay)
            async for event in inner:
                yield event

        return iterator()


async def session_for(tmp: Path, provider, history: int = 0) -> CodingSession:
    storage = JsonlSessionStorage(tmp / "session.jsonl")
    parent = None
    for i in range(history):
        user = MessageEntry(parent_id=parent, message=UserMessage(content=f"question-{i}-" + "x" * 8000))
        asst = MessageEntry(parent_id=user.id, message=AssistantMessage(content="y" * 8000, model="fake"))
        await storage.append(user)
        await storage.append(asst)
        parent = asst.id
    return await CodingSession.load(
        CodingSessionConfig(provider=provider, model="fake", system="You are Tau.", storage=storage, cwd=tmp)
    )


async def run(name: str, provider, cmds: list, history: int = 0) -> None:
    (OUT / f"{name}.cmds.jsonl").write_text("".join(json.dumps(c) + "\n" for c in cmds))
    with tempfile.TemporaryDirectory() as d:
        session = await session_for(Path(d), provider, history)
        r, w = os.pipe()

        def feed() -> None:
            with os.fdopen(w, "w") as out:
                for c in cmds:
                    if "__wait" in c:
                        time.sleep(c["__wait"])
                        continue
                    out.write((c["__raw"] if "__raw" in c else json.dumps(c)) + "\n")
                    out.flush()

        threading.Thread(target=feed, daemon=True).start()
        stdout = OutCapture()
        with os.fdopen(r, "r") as stdin:
            await RpcServer(session, stdin=stdin, stdout=stdout).run()
    (OUT / f"tau_{name}.jsonl").write_text(stdout.text)
    print(f"{name}: {stdout.text.count(chr(10))} records")


class OutCapture:
    def __init__(self) -> None:
        self.text = ""

    def write(self, s: str) -> None:
        self.text += s

    def flush(self) -> None:
        pass


async def main() -> None:
    await run("prompt", FakeProvider([reply("hello")]),
              [{"id": "one", "type": "prompt", "message": "hi"}, {"__wait": 1.0}])
    await run("state", FakeProvider([]), [
        {"id": "state", "type": "get_state"},
        {"id": "models", "type": "get_available_models"},
        {"id": "levels", "type": "get_available_thinking_levels"},
        {"id": "thinking", "type": "set_thinking_level", "level": "off"},
        {"id": "state2", "type": "get_state"},
    ])
    await run("bad_records", FakeProvider([]), [
        {"__raw": "not-json"},
        {"__raw": "[1]"},
        {"__raw": '{"id":1}'},
        {"id": 2, "type": "nope"},
        {"id": 3, "type": "get_state"},
    ])
    await run("streaming", SlowProvider([reply("first"), reply("second"), reply("third")], delay=0.6), [
        {"id": "p", "type": "prompt", "message": "start"},
        {"__wait": 0.2},
        {"id": "again", "type": "prompt", "message": "no behavior"},
        {"id": "s", "type": "steer", "message": "steer this"},
        {"id": "f", "type": "prompt", "message": "after", "streamingBehavior": "followUp"},
        {"__wait": 2.0},
    ])
    await run("compact", FakeProvider([reply("real summary", Usage(input=800, output=40, cache_read=200))]), [
        {"id": "compact", "type": "compact"},
        {"__wait": 0.5},
    ], history=15)
    await run("inspect", FakeProvider([reply("the answer")]), [
        {"id": "p", "type": "prompt", "message": "question"},
        {"__wait": 1.0},
        {"id": "stats", "type": "get_session_stats"},
        {"id": "messages", "type": "get_messages"},
        {"id": "last", "type": "get_last_assistant_text"},
    ])
    await run("set_model_fails", FakeProvider([]), [
        {"id": "model", "type": "set_model", "provider": "other", "modelId": "missing"},
        {"id": "state", "type": "get_state"},
    ])


if __name__ == "__main__":
    anyio.run(main)
