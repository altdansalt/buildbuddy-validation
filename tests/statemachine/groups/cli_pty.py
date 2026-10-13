# Exercise the released CLI's real terminal organization picker; no product mocks.
import fcntl, json, os, pty, re, select, signal, struct, sys, termios, time
binary, target, names_json, index = sys.argv[1:]
names = json.loads(names_json)
index = int(index)
pid, fd = pty.fork()
if pid == 0:
    os.execv(binary, [binary, 'ui', '--target=' + target])
fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack('HHHH', 50, 160, 0, 0))
text = ''
def wait_for(predicate):
    global text
    until = time.monotonic() + 8
    while time.monotonic() < until:
        if select.select([fd], [], [], .05)[0]:
            try:
                data = os.read(fd, 65536)
            except OSError:
                break
            if not data:
                break
            # Respond to terminal capability queries so BubbleTea needn't wait.
            if b'\x1b[6n' in data:
                os.write(fd, b'\x1b[1;1R')
            text += re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]', '', data.decode(errors='replace'))
            if predicate(text):
                return
    raise AssertionError('CLI terminal expectation failed: ' + repr(text))
try:
    wait_for(lambda s: 'Select Organization' in s and all(n in s for n in names))
    # Move to the requested model's organization regardless of default cursor.
    text = ''
    os.write(fd, b'k' * len(names) + b'j' * index)
    wait_for(lambda s: '> ' + names[index] in s)
    text = ''
    os.write(fd, b'\r')
    wait_for(lambda s: 'BuildBuddy Builds' in s and 'switch org' in s and 'org0' in s)
    assert 'Error:' not in text, text
    os.write(fd, b'q')
    print('CLI picker inventory and selection verified: ' + names[index])
finally:
    # Terminate on failure too; the outer command also has a bounded context.
    try:
        os.kill(pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    os.waitpid(pid, 0)
    os.close(fd)
