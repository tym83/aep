#!/usr/bin/env python3
"""Exercise an existing AEP change through a ServiceNow PDI; never uses Kubernetes."""
import argparse
import hashlib
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def request(base, path, token=None, method="GET", body=None, headers=None):
    url = base.rstrip("/") + path
    if urllib.parse.urlsplit(url).scheme != "https":
        raise ValueError("HTTPS is required")
    hdr = {"Accept": "application/json", **(headers or {})}
    if token:
        hdr["Authorization"] = "Bearer " + token
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        hdr["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=hdr, method=method)
    with urllib.request.build_opener(NoRedirect).open(req, timeout=30) as response:
        raw = response.read(4 * 1024 * 1024)
        return json.loads(raw) if raw else {}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--change", required=True, help="Existing organization/change-name")
    parser.add_argument("--approval", help="ServiceNow sysapproval_approver sys_id")
    parser.add_argument("--execute", action="store_true", help="Confirm using the human token; otherwise read-only")
    parser.add_argument("--timeout", type=int, default=600)
    args = parser.parse_args()
    if len(args.change.split("/")) != 2 or any(not part or not all(c.isalnum() or c in "-_" for c in part) for part in args.change.split("/")):
        parser.error("invalid change ID")
    sn = os.environ["SN_URL"]
    aep = os.environ["AEP_URL"]
    human = os.environ["AEP_HUMAN_TOKEN"]
    # Use a short-lived ServiceNow access token acquired separately; no secrets
    # appear in arguments, files, logs, or failure response bodies.
    sn_token = os.environ["SN_ACCESS_TOKEN"]
    view = request(aep, "/v1/changes/" + args.change, human)
    correlation = "aep-" + hashlib.sha256(args.change.encode()).hexdigest()
    query = urllib.parse.urlencode({"sysparm_query": "correlation_id=" + correlation, "sysparm_limit": "2"})
    records = request(sn, "/api/now/table/change_request?" + query, sn_token)["result"]
    if len(records) != 1:
        raise ValueError("expected exactly one mirrored change; check bridge reconciliation")
    record = records[0]
    print("Mirrored change:", record["sys_id"], record.get("number", ""))
    if not args.execute:
        print("Read-only check passed. CAB approval and human confirmation are still required.")
        return
    if not args.approval or not args.approval.isalnum():
        parser.error("--execute requires --approval with a ServiceNow approval sys_id")
    evidence = request(sn, "/api/now/table/sysapproval_approver/" + args.approval, sn_token)["result"]
    approved_change = evidence.get("sysapproval", "")
    if isinstance(approved_change, dict):
        approved_change = approved_change.get("value")
    if evidence.get("state") != "approved" or approved_change != record["sys_id"]:
        raise ValueError("approval must be approved and belong to this mirrored change")
    # The human reviews the view and explicitly opts into confirmation. AEP
    # checks the token groups, requester separation and the reviewed version.
    print("Confirming", args.change, "at resourceVersion", view["request"]["metadata"]["resourceVersion"])
    request(aep, "/v1/changes/" + args.change + "/decision", human, "POST",
            {"decision": "approve", "evidence": sn.rstrip("/") + "/sysapproval_approver.do?sys_id=" + args.approval},
            {"If-Match": view["request"]["metadata"]["resourceVersion"]})
    deadline = time.monotonic() + args.timeout
    while time.monotonic() < deadline:
        view = request(aep, "/v1/changes/" + args.change, human)
        phase = view["request"].get("status", {}).get("phase")
        if phase in ("Failed", "Rejected", "Expired", "Superseded"):
            raise ValueError("change ended in " + phase)
        if phase == "Applied":
            remote = request(sn, "/api/now/table/change_request/" + record["sys_id"], sn_token)["result"]
            if str(remote.get("state")) == "3" and remote.get("close_code") == "successful":
                print("PASS: AEP Applied; ServiceNow closed successfully")
                return
        time.sleep(5)
    raise TimeoutError("timed out waiting for apply and ServiceNow closure")


if __name__ == "__main__":
    try:
        main()
    except urllib.error.HTTPError as exc:
        raise SystemExit(f"Remote HTTP {exc.code}; response body omitted") from None
    except (KeyError, ValueError, TimeoutError, urllib.error.URLError) as exc:
        raise SystemExit(str(exc)) from None
