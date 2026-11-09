"""Real Windows -> WSL target protocol checks (no model calls).

Build cmd/control-server for linux/amd64 first, then pass that binary with
--binary. This checks real Linux processes, filesystem snapshots and recovery; it
does not stand in for the product scheduler's multi-task model acceptance tests.
All project and state directories are newly created and retained for inspection.
"""

import argparse
import base64
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import queue
import subprocess
import tempfile
import threading
import time


def compact(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def digest(value):
    if isinstance(value, str):
        value = value.encode("utf-8")
    return hashlib.sha256(value).hexdigest()


def timestamp(value):
    return value.isoformat().replace("+00:00", "Z")


def target_time(value):
    return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))


class Channel:
    def __init__(self, experiment):
        self.experiment = experiment
        self.sequence = 0
        self.target_id = ""
        self.clock = None
        self.sample_sent = 0
        self.queue = queue.Queue()
        self.stderr = open(experiment.artifacts / f"stderr-{len(experiment.transcript)}.txt", "wb")
        self.process = subprocess.Popen(
            experiment.wsl_args + [experiment.binary, "--mode", "supervision-target", "--data-dir", experiment.state],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=self.stderr,
            text=True, encoding="utf-8",
        )

        def read():
            for line in self.process.stdout:
                self.queue.put(line)
            self.queue.put(None)

        self.reader = threading.Thread(target=read, daemon=True)
        self.reader.start()

    def call(self, method, expected_error=None, **parameters):
        self.sequence += 1
        request = {"protocol": 1, "id": f"request-{self.sequence}", "method": method, **parameters}
        if method != "hello":
            request["targetId"] = self.target_id
        sent = time.monotonic()
        self.process.stdin.write(compact(request) + "\n")
        self.process.stdin.flush()
        line = self.queue.get(timeout=30)
        if line is None:
            raise RuntimeError(f"target channel ended, exit={self.process.poll()}")
        response = json.loads(line)
        self.experiment.transcript.append({"request": request, "response": response})
        self.experiment.save()
        assert response["protocol"] == 1 and response["id"] == request["id"], response
        if expected_error:
            assert response.get("error") == expected_error, response
            return response
        assert not response.get("error"), response
        if method == "hello":
            self.target_id = response["targetId"]
        assert response["targetId"] == self.target_id, response
        self.clock = target_time(response["targetTime"])
        self.sample_sent = sent
        return response

    def deadline(self, seconds=25):
        remaining = seconds - (time.monotonic() - self.sample_sent)
        assert remaining > 0 and seconds <= 30, "refresh stale target clock sample"
        return timestamp(self.clock + dt.timedelta(seconds=remaining))

    def close(self):
        try:
            if not self.process.stdin.closed:
                self.process.stdin.close()
            code = self.process.wait(timeout=20)
            assert code == 0, f"target did not converge: exit={code}"
        finally:
            self.stderr.close()


class Experiment:
    def __init__(self, args):
        self.wsl_args = ["wsl.exe", "-d", args.distro, "--exec"]
        self.artifacts = Path(args.artifact_dir or tempfile.mkdtemp(prefix="milevia-supervision-target-wsl-"))
        self.artifacts.mkdir(parents=True, exist_ok=True)
        self.transcript = []
        self.binary = self.command("wslpath", "-a", "-u", str(Path(args.binary).resolve())).strip()
        self.project = self.command("mktemp", "-d", "/tmp/milevia-target-project-XXXXXX").strip()
        self.state = self.command("mktemp", "-d", "/tmp/milevia-target-state-XXXXXX").strip()
        self.command("python3", "-c", "import json,pathlib,sys;root=pathlib.Path(sys.argv[1]);[(root/name).write_text(content) for name,content in json.loads(sys.argv[2]).items()]", self.project, compact({
            "entry.js": "before",
            "pass.cjs": "const a=require('node:assert/strict'),fs=require('node:fs');a.equal(fs.readFileSync('entry.js','utf8'),'before');console.log('real WSL check passed');",
            "failure.cjs": "console.error('intentional independent failure');process.exit(7);",
            "slow.cjs": "console.log('started long check');setTimeout(()=>{},60000);",
        }))
        self.channel = None
        self.version = 1
        self.plan = {"goal": "verify real WSL execution and recovery", "scope": ["entry.js"], "dependsOn": [], "acceptance": [{"id": "contents", "requirement": "verify expected contents", "kind": "command", "verification": "run actual Node check", "critical": True}], "environment": ["Node in WSL"], "review": ["compare real evidence"], "limitations": []}
        self.executions = []

    def command(self, *args):
        return subprocess.check_output(self.wsl_args + list(args), text=True, encoding="utf-8", timeout=30)

    def save(self):
        (self.artifacts / "transcript.json").write_text(json.dumps(self.transcript, ensure_ascii=False, indent=2), encoding="utf-8")

    def connect(self):
        self.channel = Channel(self)
        return self.channel.call("hello")["targetId"]

    def base(self):
        return {"sessionId": "wsl-session", "workspaceId": self.workspace["id"], "ownerEpoch": 1, "authorizationVersion": self.version}

    def control(self, state="running"):
        control = {"protocol": 1, "targetId": self.target_id, "sessionId": "wsl-session", "ownerEpoch": 1, "version": self.version, "state": state, "workspaceId": self.workspace["id"], "authorizedUntil": self.channel.deadline()}
        return self.channel.call("control", sessionId="wsl-session", control=control)

    def renew(self):
        return self.channel.call("renew", sessionId="wsl-session", ownerEpoch=1, authorizationVersion=self.version, until=self.channel.deadline())

    def snapshot(self):
        capture = self.channel.call("snapshot", **self.base())["data"]
        assert capture["manifest"]["targetOs"] == "linux", "snapshot used controller path semantics"
        return capture

    def check(self, name):
        return {"id": name, "acceptanceIds": ["contents"], "program": "node", "args": [name + ".cjs"], "directory": ".", "timeoutMillis": 10000}

    def start_check(self, name):
        self.renew()
        capture = self.snapshot()
        observation = compact({"revisionId": capture["revisionId"], "rulesHash": capture["rulesHash"], "plan": self.plan, "check": self.check(name)}).encode()
        spec = {"protocol": 1, "targetId": self.target_id, "sessionId": "wsl-session", "executionId": "execution-" + name, "workItemId": "work-" + name, "attempt": 1, "ownerEpoch": 1, "authorizationVersion": self.version, "workspaceId": self.workspace["id"], "role": "test", "agentId": "milevia", "contract": "milevia-verification-v1", "inputSha256": digest(observation), "instructionsSha256": digest("milevia-verification-v1"), "preparedAt": timestamp(self.channel.clock), "turnDeadline": self.channel.deadline(20)}
        request = {"sessionId": "wsl-session", "executionId": spec["executionId"], "spec": spec, "input": {"observation": base64.b64encode(observation).decode(), "instructions": "milevia-verification-v1"}}
        first = self.channel.call("start", **request)["execution"]
        replay = self.channel.call("start", **request)["execution"]
        assert first["container"] == replay["container"], "duplicate start created another container"
        self.executions.append(request)
        return request

    def wait(self, request):
        until = time.monotonic() + 20
        while time.monotonic() < until:
            execution = self.channel.call("query", sessionId="wsl-session", executionId=request["executionId"])["execution"]
            if execution["state"] == "quiescent":
                return execution
            time.sleep(0.1)
        raise AssertionError("target execution failed to converge")

    def wait_for_output(self, request, text):
        until = time.monotonic() + 15
        while time.monotonic() < until:
            page = self.channel.call("events", sessionId="wsl-session", after=0, limit=100)["events"]
            if any(e["executionId"] == request["executionId"] and text in compact(e["payload"]) for e in page["events"]):
                return
            time.sleep(0.1)
        raise AssertionError("real long-running check never produced its startup output")

    def result(self, request):
        execution = self.wait(request)
        events = self.channel.call("events", sessionId="wsl-session", after=0, limit=100)["events"]["events"]
        results = [e for e in events if e["executionId"] == request["executionId"] and e["type"] == "verification.result"]
        assert len(results) == 1, results
        result = results[0]["payload"]
        assert digest(compact(result)) == execution["resultSha256"], "result does not bind persisted evidence"
        assert not result.get("notExecuted"), result
        return execution, result

    def run(self):
        self.target_id = self.connect()
        self.workspace = self.channel.call("workspace", path=self.project)["data"]
        self.control()
        passed = self.start_check("pass")
        execution, result = self.result(passed)
        assert not execution["interrupted"] and result["exitCode"] == 0, result
        failed = self.start_check("failure")
        execution, result = self.result(failed)
        assert execution["interrupted"] and result["exitCode"] == 7, result
        # Failure of one independent check must not poison the target grant.
        self.snapshot()
        # Start a long real check, disconnect its owner pipe and query the same
        # execution after restart. No new execution ID or repeated side effect.
        slow = self.start_check("slow")
        self.wait_for_output(slow, "started long check")
        self.channel.close()
        self.channel = None
        assert self.connect() == self.target_id
        interrupted = self.wait(slow)
        assert interrupted["interrupted"], interrupted
        replay = self.channel.call("start", **slow)["execution"]
        assert replay == interrupted, "reconnect replay changed execution facts"
        self.version += 1
        self.control()
        self.channel.call("start", expected_error="authority", **{**slow, "executionId": "late-old-execution", "spec": {**slow["spec"], "executionId": "late-old-execution"}})
        capture = self.snapshot()
        environments = self.channel.call("environment", parameters={"plan": self.plan, "checks": [self.check("pass")]}, **self.base())["data"]
        final = {"id": "wsl-final", **self.base(), "bindingSha256": digest("selected-task-evidence-watermark"), "revisionId": capture["revisionId"], "rulesHash": capture["rulesHash"], "environmentHash": environments["sha256"]}
        payload = {"spec": final, "checks": [self.check("pass")]}
        receipt = self.channel.call("finalize", parameters=payload, **self.base())["data"]
        assert receipt["state"] == "sealed", receipt
        assert self.channel.call("finalize", parameters=payload, **self.base())["data"] == receipt
        self.version += 1
        self.control("stopped")
        for _ in range(2):
            self.channel.call("release", sessionId="wsl-session", ownerEpoch=1, authorizationVersion=self.version)
        self.channel.close()
        self.channel = None
        assert self.connect() == self.target_id
        session = self.channel.call("query", sessionId="wsl-session")["session"]
        assert session["released"] and session["finalization"] == receipt, session
        assert self.channel.call("start", **passed)["execution"]["state"] == "quiescent"
        return {"passed": True, "environment": "Windows -> WSL", "project": self.project, "stateDirectory": self.state, "artifacts": str(self.artifacts), "realChecks": 3, "duplicateStartDeduplicated": True, "failedExitCodePreserved": True, "disconnectStoppedRealProcess": True, "oldAuthorizationRejected": True, "finalReceiptSurvivedRestart": True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, help="Windows path to a Linux control-server binary")
    parser.add_argument("--distro", default="Ubuntu-24.04")
    parser.add_argument("--artifact-dir")
    args = parser.parse_args()
    if os.name != "nt":
        parser.error("run from Windows with wsl.exe available")
    experiment = Experiment(args)
    try:
        summary = experiment.run()
    except Exception as error:
        summary = {"passed": False, "error": str(error), "artifacts": str(experiment.artifacts)}
        raise
    finally:
        experiment.save()
        (experiment.artifacts / "summary.json").write_text(json.dumps(summary, ensure_ascii=False, indent=2), encoding="utf-8")
        if experiment.channel is not None:
            experiment.channel.close()
    print(json.dumps(summary, ensure_ascii=False))


if __name__ == "__main__":
    main()
