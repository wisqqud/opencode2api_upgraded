package protocol

import (
	"encoding/json"
	"fmt"
	"strings"

	"opencode2api/internal/jsonutil"
)

// EnableShellBridge enables filesystem adaptation only for a registered command
// tool. Commands are returned to the client, never executed by the gateway.
func (r *ToolRenames) EnableShellBridge(client string) {
	if r != nil {
		if _, ok := r.Wire(client); ok {
			r.shellBridge = client
		}
	}
}

func (r *ToolRenames) HasShellBridge() bool { return r != nil && r.shellBridge != "" }

func (r *ToolRenames) CanExecuteOfficial(name string) bool {
	return r.AllowsResponseTool(name) || (r.HasShellBridge() && filesystemScript(name) != "")
}

func toolBridgeError(name, reason string) error {
	return fmt.Errorf("tool compatibility error: %s: %s", name, reason)
}

// AdaptResponseTools checks a complete response before the legacy rename/filter
// pass. Opaque aliases already use client schemas and must stay untouched.
func AdaptResponseTools(body []byte, protocol Protocol, r *ToolRenames) ([]byte, error) {
	if !r.HasShellBridge() {
		return body, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if err := adaptResponseMap(payload, protocol, r); err != nil {
		return nil, err
	}
	return json.Marshal(payload)
}

func adaptResponseMap(payload map[string]any, protocol Protocol, r *ToolRenames) error {
	adapt := func(call map[string]any) error {
		return r.adaptToolCall(call, protocol == Anthropic)
	}
	switch protocol {
	case Chat:
		for _, raw := range jsonutil.SliceAt(payload, "choices") {
			choice, _ := raw.(map[string]any)
			message := jsonutil.MapAt(choice, "message")
			for _, rawCall := range jsonutil.SliceAt(message, "tool_calls") {
				call, _ := rawCall.(map[string]any)
				if err := adapt(jsonutil.MapAt(call, "function")); err != nil {
					return err
				}
			}
			if fn := jsonutil.MapAt(message, "function_call"); fn != nil {
				if err := adapt(fn); err != nil {
					return err
				}
			}
		}
	case Responses:
		for _, raw := range jsonutil.SliceAt(payload, "output") {
			call, _ := raw.(map[string]any)
			if typ := jsonutil.StringAt(call, "type"); typ == "function_call" || typ == "custom_tool_call" || typ == "tool_call" {
				if err := adapt(call); err != nil {
					return err
				}
			}
		}
		if response := jsonutil.MapAt(payload, "response"); response != nil {
			return adaptResponseMap(response, protocol, r)
		}
	case Anthropic:
		for _, raw := range jsonutil.SliceAt(payload, "content") {
			call, _ := raw.(map[string]any)
			if jsonutil.StringAt(call, "type") == "tool_use" {
				if err := adapt(call); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (r *ToolRenames) adaptToolCall(call map[string]any, nativeInput bool) error {
	name := jsonutil.StringAt(call, "name")
	if r.AllowsResponseTool(name) {
		return nil
	}
	script := filesystemScript(name)
	if script == "" {
		return toolBridgeError(name, "no executable client tool")
	}
	raw := jsonutil.StringAt(call, "arguments")
	if nativeInput {
		b, err := json.Marshal(call["input"])
		if err != nil {
			return toolBridgeError(name, "invalid input")
		}
		raw = string(b)
	}
	args, err := filesystemArguments(name, raw)
	if err != nil {
		return toolBridgeError(name, err.Error())
	}
	encoded, _ := json.Marshal(args)
	command := "python3 -c " + quoteShell(script) + " " + quoteShell(string(encoded))
	clientArgs := map[string]any{"cmd": command}
	call["name"], _ = r.Wire(r.shellBridge)
	if nativeInput {
		call["input"] = clientArgs
	} else {
		b, _ := json.Marshal(clientArgs)
		call["arguments"] = string(b)
	}
	return nil
}

func quoteShell(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func filesystemArguments(name, raw string) (map[string]any, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil || args == nil {
		return nil, fmt.Errorf("invalid argument object")
	}
	allowed := map[string]string{"path": "string", "limit": "number"}
	required := "pattern"
	switch name {
	case "read":
		allowed["offset"] = "number"
		required = "path"
	case "glob":
		allowed["pattern"] = "string"
		allowed["hidden"] = "boolean"
	case "grep":
		allowed["pattern"] = "string"
		allowed["include"] = "string"
		allowed["literal"] = "boolean"
		allowed["caseSensitive"] = "boolean"
	}
	for key, value := range args {
		switch allowed[key] {
		case "string":
			s, ok := value.(string)
			if !ok || strings.ContainsRune(s, 0) {
				return nil, fmt.Errorf("invalid %s", key)
			}
		case "boolean":
			if _, ok := value.(bool); !ok {
				return nil, fmt.Errorf("invalid %s", key)
			}
		case "number":
			n, ok := value.(float64)
			if !ok || n < 0 || n > 1e9 || n != float64(int64(n)) || ((key == "offset" || name != "read") && n == 0) {
				return nil, fmt.Errorf("invalid %s", key)
			}
		default:
			return nil, fmt.Errorf("unsupported argument %s", key)
		}
	}
	if s, _ := args[required].(string); s == "" {
		return nil, fmt.Errorf("missing %s", required)
	}
	return args, nil
}

// Each helper receives JSON as an argv value: paths/patterns never become shell
// syntax. Python and ripgrep run inside the client's normal execution sandbox.
func filesystemScript(name string) string {
	switch name {
	case "read":
		return readScript
	case "glob":
		return globScript
	case "grep":
		return grepScript
	default:
		return ""
	}
}

const readScript = `import sys,json,pathlib,itertools
a=json.loads(sys.argv[1]);p=pathlib.Path(a["path"]).expanduser();start=a.get("offset",1);limit=min(a.get("limit",2000),2000)
if p.is_dir():
 for x in itertools.islice(sorted(p.iterdir()),start-1,start-1+limit):print(x.name+("/" if x.is_dir() else ""))
else:
 with p.open("rb") as f:
  head=f.read(8192)
  if b"\0" in head or head.startswith((b"%PDF-",b"\x89PNG",b"\xff\xd8\xff",b"GIF8",b"RIFF")):sys.exit("read: binary/image/PDF input requires a dedicated client tool")
 with p.open(encoding="utf-8",errors="replace") as f:
  for n,line in enumerate(itertools.islice(f,start-1,start-1+limit),start):print(str(n)+": "+line.rstrip("\r\n"))`

const globScript = `import sys,json,subprocess
a=json.loads(sys.argv[1]);cmd=["rg","--no-config","--files","--null","-g",a["pattern"]]
if a.get("hidden",False):cmd.append("--hidden")
else:cmd += ["-g","!**/.*"]
cmd += ["--",a.get("path") or "."]
p=subprocess.run(cmd,stdout=subprocess.PIPE)
if p.returncode not in (0,1):sys.exit(p.returncode)
for x in sorted(p.stdout.split(b"\0")[:-1])[:a.get("limit",100)]:print(x.decode("utf-8","replace"))`

const grepScript = `import sys,json,subprocess
a=json.loads(sys.argv[1]);cmd=["rg","--no-config","--json","--color","never","--case-sensitive"]
if a.get("literal",False):cmd.append("--fixed-strings")
if not a.get("caseSensitive",True):cmd.append("--ignore-case")
if a.get("include"):cmd += ["-g",a["include"]]
cmd += ["-g","!**/.*"]
cmd += ["-e",a["pattern"],"--",a.get("path") or "."]
p=subprocess.Popen(cmd,stdout=subprocess.PIPE);count=0;limited=False
try:
 for raw in p.stdout:
  e=json.loads(raw)
  if e["type"]!="match":continue
  d=e["data"];print(d["path"].get("text","<binary path>")+":"+str(d["line_number"])+": "+d["lines"].get("text","<binary line>").rstrip("\r\n"));count+=1
  if count>=a.get("limit",100):limited=True;break
finally:
 if limited:p.terminate()
 p.stdout.close();code=p.wait()
if not limited and code not in (0,1):sys.exit(code)`
