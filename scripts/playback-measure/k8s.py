#!/usr/bin/env python3
"""Replay two append slices using isolated Jobs on an existing media PVC."""
import argparse
import json
import subprocess
import time
import uuid
from pathlib import Path


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--namespace", default="rokuban")
    p.add_argument("--deployment", default="rokuban")
    p.add_argument("--writer-node", required=True)
    p.add_argument("--reader-node", required=True)
    p.add_argument("--probe", type=Path, required=True)
    p.add_argument("--source", type=Path, required=True, help="280-second constant-muxrate TS")
    p.add_argument("--output", type=Path, required=True)
    args = p.parse_args()
    if args.writer_node == args.reader_node:
        p.error("writer and reader must use different nodes")
    args.output.mkdir(parents=True, exist_ok=False)
    name = "rp3-measure-" + uuid.uuid4().hex[:10]
    root = "/media/." + name
    kubectl = ["kubectl", "-n", args.namespace]

    def k(*command, **kwargs):
        return subprocess.run(kubectl + list(command), check=True, **kwargs)

    def json_get(*command):
        return json.loads(k(*command, capture_output=True, text=True).stdout)

    def remote(pod, *command):
        return k("exec", pod, "--", *command, capture_output=True, text=True).stdout

    deployment = json_get("get", "deployment", args.deployment, "-o", "json")
    template = deployment["spec"]["template"]["spec"]
    # Only mount the PVC, never the application's config, DB secrets or devices.
    media = next(v for v in template["volumes"] if v["name"] == "media")
    if "persistentVolumeClaim" not in media:
        raise ValueError("deployment media volume is not a PVC")
    common = {"restartPolicy": "Never", "securityContext": template["securityContext"],
              "imagePullSecrets": template.get("imagePullSecrets", []),
              "volumes": [media, {"name": "scratch", "emptyDir": {}}]}
    pods = []
    for part, node in (("writer", args.writer_node), ("reader", args.reader_node)):
        spec = json.loads(json.dumps(common))
        spec["nodeName"] = node
        spec["containers"] = [{"name": "probe", "image": template["containers"][0]["image"],
                               "imagePullPolicy": "IfNotPresent", "command": ["sleep", "3600"],
                               "resources": {"requests": {"cpu": "50m", "memory": "64Mi"},
                                             "limits": {"cpu": "1", "memory": "256Mi"}},
                               "volumeMounts": [{"name": "media", "mountPath": "/media",
                                                 "readOnly": part == "reader"},
                                                {"name": "scratch", "mountPath": "/tmp"}]}]
        pods.append({"apiVersion": "v1", "kind": "Pod",
                     "metadata": {"name": name + "-" + part, "labels": {"rp3": name}},
                     "spec": spec})
    policy = {"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
              "metadata": {"name": name}, "spec": {"podSelector": {"matchLabels": {"rp3": name}},
              "policyTypes": ["Ingress", "Egress"],
              "ingress": [{"from": [{"podSelector": {"matchLabels": {"rp3": name}}}],
                           "ports": [{"protocol": "TCP", "port": 18080}]}],
              "egress": [{"to": [{"podSelector": {"matchLabels": {"rp3": name}}}],
                          "ports": [{"protocol": "TCP", "port": 18080}]}]}}
    manifests = {"apiVersion": "v1", "kind": "List", "items": pods + [policy]}
    path = args.output / "resources.json"
    path.write_text(json.dumps(manifests, indent=2))
    writer, reader = [pod["metadata"]["name"] for pod in pods]
    print(f"reader={reader} root={root}", flush=True)
    processes = []
    try:
        k("apply", "-f", str(path), capture_output=True)
        k("wait", "--for=condition=Ready", "pod", "-l", "rp3=" + name, "--timeout=120s")
        for pod in (writer, reader):
            k("cp", str(args.probe), pod + ":/tmp/probe", capture_output=True)
        k("cp", str(args.source), writer + ":/tmp/source.ts", capture_output=True)
        remote(writer, "sh", "-c", f"mkdir {root}; cp /tmp/probe {root}/probe; cp /tmp/source.ts {root}/source.ts")
        ip = json_get("get", "pod", writer, "-o", "json")["status"]["podIP"]
        clock = subprocess.Popen(kubectl + ["exec", writer, "--", "/tmp/probe", "clock"],
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        processes.append(clock)
        time.sleep(1)
        (args.output / "clock-before.jsonl").write_text(remote(reader, "/tmp/probe", "calibrate", f"http://{ip}:18080"))
        with open(args.output / "reader.jsonl", "w") as stdout, open(args.output / "reader.stderr", "w") as stderr:
            process = subprocess.Popen(kubectl + ["exec", reader, "--", "/tmp/probe", "read", root + "/growing.ts", "360"], stdout=stdout, stderr=stderr)
            processes.append(process)
            for part, start, end in (("a", 0, 480), ("b", 480, 560)):
                spec = json.loads(json.dumps(pods[0]["spec"]))
                spec["containers"][0]["command"] = [root + "/probe", "write", root + "/growing.ts", root + "/source.ts", str(start), str(end), "560"]
                job_name = name + "-" + part
                job = {"apiVersion": "batch/v1", "kind": "Job", "metadata": {"name": job_name, "labels": {"rp3": name}},
                       "spec": {"backoffLimit": 0, "activeDeadlineSeconds": 300,
                                "template": {"metadata": {"labels": {"rp3": name}}, "spec": spec}}}
                job_path = args.output / ("job-" + part + ".json")
                job_path.write_text(json.dumps(job, indent=2))
                k("apply", "-f", str(job_path), capture_output=True)
                print("created", job_name, flush=True)
                while True:
                    status = json_get("get", "job", job_name, "-o", "json")["status"]
                    if status.get("failed"):
                        raise RuntimeError(status)
                    if status.get("succeeded"):
                        break
                    time.sleep(1)
                (args.output / ("writer-" + part + ".jsonl")).write_text(k("logs", "job/" + job_name, capture_output=True, text=True).stdout)
            (args.output / "clock-after.jsonl").write_text(remote(reader, "/tmp/probe", "calibrate", f"http://{ip}:18080"))
            if process.wait() != 0:
                raise RuntimeError("reader failed")
    finally:
        for process in processes:
            if process.poll() is None:
                process.terminate()
        # The root is an internally generated, unique name. Never delete input paths.
        try:
            k("exec", writer, "--", "rm", "-rf", root, capture_output=True)
        finally:
            k("delete", "job,pod,networkpolicy", "-l", "rp3=" + name, "--wait=false", capture_output=True)


if __name__ == "__main__":
    main()
