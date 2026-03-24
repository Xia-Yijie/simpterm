#define _DEFAULT_SOURCE
#define _XOPEN_SOURCE 600

#include <ctype.h>
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <pwd.h>
#include <signal.h>
#include <stdarg.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <termios.h>
#include <unistd.h>

#include <pty.h>
#include <time.h>

#define NAME_MAX_LEN 64
#define MSG_MAX_LEN 256
#define CMD_MAX_LEN 4096
#define PAYLOAD_MAX 4096
#define MARKER_LEN 32
#define BACKLOG_LIMIT (1024 * 1024)
#define MAX_SESSIONS 128

enum ctl_cmd {
    CMD_NEW = 1,
    CMD_LIST = 2,
    CMD_KILL = 3,
    CMD_ATTACH = 4,
    CMD_EXEC = 5
};

enum pkt_type {
    PKT_DATA = 1,
    PKT_RESIZE = 2,
    PKT_EXIT = 3
};

struct ctl_req {
    int cmd;
    int id;
    struct winsize ws;
    char name[NAME_MAX_LEN];
    char data[CMD_MAX_LEN];
};

struct ctl_resp {
    int status;
    int id;
    pid_t pid;
    int more;
    char name[NAME_MAX_LEN];
    char msg[MSG_MAX_LEN];
    char marker[MARKER_LEN];
};

struct packet {
    uint32_t type;
    uint32_t len;
    char data[PAYLOAD_MAX];
};

struct session {
    int used;
    int id;
    pid_t pid;
    int pty_fd;
    int client_fd;
    int exec_fd;
    char name[NAME_MAX_LEN];
    char *backlog;
    size_t backlog_len;
    size_t backlog_cap;
};

static struct session sessions[MAX_SESSIONS];
static int next_session_id = 1;

static struct termios saved_termios;
static int termios_saved = 0;
static volatile sig_atomic_t resize_pending = 0;

static void die(const char *fmt, ...) {
    va_list ap;
    va_start(ap, fmt);
    vfprintf(stderr, fmt, ap);
    va_end(ap);
    fputc('\n', stderr);
    exit(1);
}

static void format_runtime_dir(char *buf, size_t size) {
    int n = snprintf(buf, size, "/tmp/simpterm-%u", (unsigned)getuid());
    if (n < 0 || (size_t)n >= size) {
        die("runtime dir path too long");
    }
}

static void format_socket_path(char *buf, size_t size) {
    char dir[96];
    int n;

    format_runtime_dir(dir, sizeof(dir));
    n = snprintf(buf, size, "%s/daemon.sock", dir);
    if (n < 0 || (size_t)n >= size) {
        die("socket path too long");
    }
}

static void ensure_runtime_dir(void) {
    char dir[108];
    struct stat st;

    format_runtime_dir(dir, sizeof(dir));
    if (stat(dir, &st) == 0) {
        if (!S_ISDIR(st.st_mode)) {
            die("%s exists but is not a directory", dir);
        }
        chmod(dir, 0700);
        return;
    }
    if (mkdir(dir, 0700) < 0 && errno != EEXIST) {
        die("mkdir %s failed: %s", dir, strerror(errno));
    }
}

static int set_nonblocking(int fd) {
    int flags = fcntl(fd, F_GETFL, 0);
    if (flags < 0) {
        return -1;
    }
    return fcntl(fd, F_SETFL, flags | O_NONBLOCK);
}

static int is_numeric(const char *s) {
    size_t i;

    if (s == NULL || *s == '\0') {
        return 0;
    }
    for (i = 0; s[i]; i++) {
        if (!isdigit((unsigned char)s[i])) {
            return 0;
        }
    }
    return 1;
}

static int connect_daemon(void) {
    char path[108];
    struct sockaddr_un addr;
    int fd;

    format_socket_path(path, sizeof(path));
    fd = socket(AF_UNIX, SOCK_SEQPACKET, 0);
    if (fd < 0) {
        return -1;
    }

    memset(&addr, 0, sizeof(addr));
    addr.sun_family = AF_UNIX;
    memcpy(addr.sun_path, path, strlen(path) + 1);

    if (connect(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        close(fd);
        return -1;
    }
    return fd;
}

static void spawn_daemon(void) {
    pid_t pid;
    int i;

    pid = fork();
    if (pid < 0) {
        die("fork failed: %s", strerror(errno));
    }
    if (pid > 0) {
        return;
    }

    if (setsid() < 0) {
        _exit(1);
    }

    pid = fork();
    if (pid < 0) {
        _exit(1);
    }
    if (pid > 0) {
        _exit(0);
    }

    if (chdir("/") < 0) {
        _exit(1);
    }
    for (i = 0; i < 3; i++) {
        close(i);
    }
    if (open("/dev/null", O_RDWR) < 0) {
        _exit(1);
    }
    if (dup(0) < 0 || dup(0) < 0) {
        _exit(1);
    }
    execl("/proc/self/exe", "simpterm", "__daemon", (char *)NULL);
    _exit(1);
}

static void ensure_daemon_running(void) {
    int fd;
    int i;

    fd = connect_daemon();
    if (fd >= 0) {
        close(fd);
        return;
    }

    spawn_daemon();
    for (i = 0; i < 50; i++) {
        usleep(100000);
        fd = connect_daemon();
        if (fd >= 0) {
            close(fd);
            return;
        }
    }
    die("failed to start daemon");
}

static ssize_t send_msg(int fd, const void *buf, size_t len) {
    int retries = 0;
    for (;;) {
        ssize_t n = send(fd, buf, len, 0);
        if (n < 0 && errno == EINTR) {
            continue;
        }
        if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) {
            if (++retries > 100) {
                return -1;
            }
            usleep(1000);
            continue;
        }
        return n;
    }
}

static ssize_t recv_msg(int fd, void *buf, size_t len) {
    for (;;) {
        ssize_t n = recv(fd, buf, len, 0);
        if (n < 0 && errno == EINTR) {
            continue;
        }
        return n;
    }
}

static int send_ctl_req(int fd, const struct ctl_req *req) {
    return send_msg(fd, req, sizeof(*req)) == (ssize_t)sizeof(*req) ? 0 : -1;
}

static int recv_ctl_req(int fd, struct ctl_req *req) {
    ssize_t n = recv_msg(fd, req, sizeof(*req));
    return n == (ssize_t)sizeof(*req) ? 0 : -1;
}

static int send_ctl_resp(int fd, const struct ctl_resp *resp) {
    return send_msg(fd, resp, sizeof(*resp)) == (ssize_t)sizeof(*resp) ? 0 : -1;
}

static int recv_ctl_resp(int fd, struct ctl_resp *resp) {
    ssize_t n = recv_msg(fd, resp, sizeof(*resp));
    return n == (ssize_t)sizeof(*resp) ? 0 : -1;
}

static int send_packet(int fd, uint32_t type, const void *data, uint32_t len) {
    struct packet pkt;

    if (len > PAYLOAD_MAX) {
        errno = EMSGSIZE;
        return -1;
    }
    memset(&pkt, 0, sizeof(pkt));
    pkt.type = type;
    pkt.len = len;
    if (len > 0 && data != NULL) {
        memcpy(pkt.data, data, len);
    }
    return send_msg(fd, &pkt, sizeof(pkt)) == (ssize_t)sizeof(pkt) ? 0 : -1;
}

static int recv_packet(int fd, struct packet *pkt) {
    ssize_t n = recv_msg(fd, pkt, sizeof(*pkt));
    if (n == 0) {
        return 0;
    }
    if (n != (ssize_t)sizeof(*pkt)) {
        return -1;
    }
    if (pkt->len > PAYLOAD_MAX) {
        errno = EPROTO;
        return -1;
    }
    return 1;
}

static struct session *find_session_by_slot(int slot) {
    if (slot < 0 || slot >= MAX_SESSIONS || !sessions[slot].used) {
        return NULL;
    }
    return &sessions[slot];
}

static struct session *find_session(const char *name, int id) {
    int i;

    for (i = 0; i < MAX_SESSIONS; i++) {
        if (!sessions[i].used) {
            continue;
        }
        if (id > 0 && sessions[i].id == id) {
            return &sessions[i];
        }
        if (name[0] && strcmp(sessions[i].name, name) == 0) {
            return &sessions[i];
        }
    }
    return NULL;
}

static void session_append_backlog(struct session *s, const char *buf, size_t len) {
    size_t needed;
    size_t drop;

    if (len == 0) {
        return;
    }
    needed = s->backlog_len + len;
    if (needed > BACKLOG_LIMIT) {
        drop = needed - BACKLOG_LIMIT;
        if (drop >= s->backlog_len) {
            s->backlog_len = 0;
        } else {
            memmove(s->backlog, s->backlog + drop, s->backlog_len - drop);
            s->backlog_len -= drop;
        }
    }

    needed = s->backlog_len + len;
    if (needed > s->backlog_cap) {
        size_t new_cap = s->backlog_cap ? s->backlog_cap : 8192;
        while (new_cap < needed) {
            new_cap *= 2;
        }
        if (new_cap > BACKLOG_LIMIT) {
            new_cap = BACKLOG_LIMIT;
        }
        char *new_buf = realloc(s->backlog, new_cap);
        if (!new_buf) {
            return;
        }
        s->backlog = new_buf;
        s->backlog_cap = new_cap;
    }
    memcpy(s->backlog + s->backlog_len, buf, len);
    s->backlog_len += len;
}

static void session_detach_client(struct session *s) {
    if (s->client_fd >= 0) {
        close(s->client_fd);
        s->client_fd = -1;
    }
}

static void session_cleanup(struct session *s) {
    if (!s->used) {
        return;
    }
    session_detach_client(s);
    if (s->exec_fd >= 0) {
        close(s->exec_fd);
    }
    if (s->pty_fd >= 0) {
        close(s->pty_fd);
    }
    free(s->backlog);
    memset(s, 0, sizeof(*s));
    s->pty_fd = -1;
    s->client_fd = -1;
    s->exec_fd = -1;
}

static struct session *session_create(const char *requested_name, struct winsize ws, char *err, size_t err_size) {
    int i;
    int pty_fd = -1;
    pid_t pid;
    const char *shell;
    struct passwd *pw;
    struct session *s = NULL;

    for (i = 0; i < MAX_SESSIONS; i++) {
        if (!sessions[i].used) {
            s = &sessions[i];
            break;
        }
    }
    if (!s) {
        snprintf(err, err_size, "too many sessions");
        return NULL;
    }

    pid = forkpty(&pty_fd, NULL, NULL, &ws);
    if (pid < 0) {
        snprintf(err, err_size, "forkpty failed: %s", strerror(errno));
        return NULL;
    }
    if (pid == 0) {
        shell = getenv("SHELL");
        if (!shell || shell[0] == '\0') {
            pw = getpwuid(getuid());
            shell = pw && pw->pw_shell && pw->pw_shell[0] ? pw->pw_shell : "/bin/sh";
        }
        execlp(shell, shell, (char *)NULL);
        _exit(127);
    }

    memset(s, 0, sizeof(*s));
    s->used = 1;
    s->id = next_session_id++;
    s->pid = pid;
    s->pty_fd = pty_fd;
    s->client_fd = -1;
    s->exec_fd = -1;
    if (requested_name && requested_name[0]) {
        snprintf(s->name, sizeof(s->name), "%s", requested_name);
    } else {
        snprintf(s->name, sizeof(s->name), "s%d", s->id);
    }
    set_nonblocking(s->pty_fd);
    return s;
}

static void reap_children(void) {
    int status;
    pid_t pid;
    int i;

    while ((pid = waitpid(-1, &status, WNOHANG)) > 0) {
        for (i = 0; i < MAX_SESSIONS; i++) {
            if (sessions[i].used && sessions[i].pid == pid) {
                if (sessions[i].client_fd >= 0) {
                    send_packet(sessions[i].client_fd, PKT_EXIT, NULL, 0);
                }
                if (sessions[i].exec_fd >= 0) {
                    send_packet(sessions[i].exec_fd, PKT_EXIT, NULL, 0);
                }
                session_cleanup(&sessions[i]);
                break;
            }
        }
    }
}

static void write_all_fd(int fd, const char *buf, size_t len) {
    size_t off = 0;
    int retries = 0;

    while (off < len) {
        ssize_t n = write(fd, buf + off, len - off);
        if (n < 0) {
            if (errno == EINTR) {
                continue;
            }
            if (errno == EAGAIN || errno == EWOULDBLOCK) {
                if (++retries > 50) {
                    break;
                }
                usleep(1000);
                continue;
            }
            break;
        }
        if (n == 0) {
            break;
        }
        retries = 0;
        off += (size_t)n;
    }
}

static int daemon_socket_listen(void) {
    char path[108];
    struct sockaddr_un addr;
    int fd;

    ensure_runtime_dir();
    format_socket_path(path, sizeof(path));
    unlink(path);

    fd = socket(AF_UNIX, SOCK_SEQPACKET, 0);
    if (fd < 0) {
        return -1;
    }

    memset(&addr, 0, sizeof(addr));
    addr.sun_family = AF_UNIX;
    memcpy(addr.sun_path, path, strlen(path) + 1);

    if (bind(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        close(fd);
        return -1;
    }
    if (listen(fd, 16) < 0) {
        close(fd);
        return -1;
    }
    return fd;
}

static void handle_list(int client_fd) {
    struct ctl_resp resp;
    int i;

    reap_children();
    for (i = 0; i < MAX_SESSIONS; i++) {
        if (!sessions[i].used) {
            continue;
        }
        memset(&resp, 0, sizeof(resp));
        resp.status = 0;
        resp.id = sessions[i].id;
        resp.pid = sessions[i].pid;
        resp.more = 1;
        snprintf(resp.name, sizeof(resp.name), "%s", sessions[i].name);
        send_ctl_resp(client_fd, &resp);
    }
    memset(&resp, 0, sizeof(resp));
    resp.status = 0;
    resp.more = 0;
    send_ctl_resp(client_fd, &resp);
    close(client_fd);
}

static void handle_kill(int client_fd, struct ctl_req *req) {
    struct ctl_resp resp;
    struct session *s = find_session(req->name, req->id);
    int i;

    memset(&resp, 0, sizeof(resp));
    if (!s) {
        resp.status = -1;
        snprintf(resp.msg, sizeof(resp.msg), "session not found");
    } else if (kill(-s->pid, SIGHUP) < 0 && errno != ESRCH) {
        resp.status = -1;
        snprintf(resp.msg, sizeof(resp.msg), "kill failed: %s", strerror(errno));
    } else if (kill(-s->pid, SIGTERM) < 0 && errno != ESRCH) {
        resp.status = -1;
        snprintf(resp.msg, sizeof(resp.msg), "kill failed: %s", strerror(errno));
    } else {
        for (i = 0; i < 50; i++) {
            usleep(10000);
            reap_children();
            if (!find_session(req->name, req->id)) {
                break;
            }
        }
        s = find_session(req->name, req->id);
        if (s && kill(-s->pid, SIGKILL) == 0) {
            for (i = 0; i < 50; i++) {
                usleep(10000);
                reap_children();
                if (!find_session(req->name, req->id)) {
                    break;
                }
            }
        }
        resp.status = 0;
        if (!s) {
            resp.id = req->id;
            snprintf(resp.name, sizeof(resp.name), "%s", req->name);
        } else {
            resp.id = s->id;
            snprintf(resp.name, sizeof(resp.name), "%s", s->name);
        }
    }
    send_ctl_resp(client_fd, &resp);
    close(client_fd);
}

static void handle_new(int client_fd, struct ctl_req *req) {
    struct ctl_resp resp;
    struct session *s;
    char err[MSG_MAX_LEN];

    memset(&resp, 0, sizeof(resp));
    if (req->name[0] && is_numeric(req->name)) {
        resp.status = -1;
        snprintf(resp.msg, sizeof(resp.msg), "session name cannot be purely numeric");
    } else if (req->name[0] && find_session(req->name, 0)) {
        resp.status = -1;
        snprintf(resp.msg, sizeof(resp.msg), "session name already exists");
    } else {
        s = session_create(req->name, req->ws, err, sizeof(err));
        if (!s) {
            resp.status = -1;
            snprintf(resp.msg, sizeof(resp.msg), "%s", err);
        } else {
            resp.status = 0;
            resp.id = s->id;
            resp.pid = s->pid;
            snprintf(resp.name, sizeof(resp.name), "%s", s->name);
        }
    }
    send_ctl_resp(client_fd, &resp);
    close(client_fd);
}

static void flush_backlog_to_client(struct session *s) {
    size_t off = 0;

    while (off < s->backlog_len) {
        uint32_t chunk = (uint32_t)(s->backlog_len - off);
        if (chunk > PAYLOAD_MAX) {
            chunk = PAYLOAD_MAX;
        }
        if (send_packet(s->client_fd, PKT_DATA, s->backlog + off, chunk) < 0) {
            session_detach_client(s);
            return;
        }
        off += chunk;
    }
}

static void handle_attach(int client_fd, struct ctl_req *req) {
    struct ctl_resp resp;
    struct session *s = find_session(req->name, req->id);

    memset(&resp, 0, sizeof(resp));
    if (!s) {
        resp.status = -1;
        snprintf(resp.msg, sizeof(resp.msg), "session not found");
        send_ctl_resp(client_fd, &resp);
        close(client_fd);
        return;
    }
    if (s->client_fd >= 0) {
        resp.status = -1;
        snprintf(resp.msg, sizeof(resp.msg), "session already attached");
        send_ctl_resp(client_fd, &resp);
        close(client_fd);
        return;
    }

    if (ioctl(s->pty_fd, TIOCSWINSZ, &req->ws) < 0) {
        resp.status = -1;
        snprintf(resp.msg, sizeof(resp.msg), "failed to set window size");
        send_ctl_resp(client_fd, &resp);
        close(client_fd);
        return;
    }

    set_nonblocking(client_fd);
    s->client_fd = client_fd;
    resp.status = 0;
    resp.id = s->id;
    resp.pid = s->pid;
    snprintf(resp.name, sizeof(resp.name), "%s", s->name);
    if (send_ctl_resp(client_fd, &resp) < 0) {
        session_detach_client(s);
        return;
    }
    flush_backlog_to_client(s);
}

static void generate_marker(char *buf, size_t size) {
    unsigned long r = 0;
    int fd = open("/dev/urandom", O_RDONLY);
    if (fd >= 0) {
        if (read(fd, &r, sizeof(r)) < 0) { r = (unsigned long)getpid(); }
        close(fd);
    }
    snprintf(buf, size, "__SIMPTERM_DONE_%lx__", r);
}

static void handle_exec(int client_fd, struct ctl_req *req) {
    struct ctl_resp resp;
    struct session *s = find_session(req->name, req->id);
    char marker[MARKER_LEN];
    char wrapped[CMD_MAX_LEN + MARKER_LEN + 32];

    memset(&resp, 0, sizeof(resp));
    if (!s) {
        resp.status = -1;
        snprintf(resp.msg, sizeof(resp.msg), "session not found");
        send_ctl_resp(client_fd, &resp);
        close(client_fd);
        return;
    }
    if (s->exec_fd >= 0) {
        resp.status = -1;
        snprintf(resp.msg, sizeof(resp.msg), "session has pending exec");
        send_ctl_resp(client_fd, &resp);
        close(client_fd);
        return;
    }
    if (!req->data[0]) {
        resp.status = -1;
        snprintf(resp.msg, sizeof(resp.msg), "empty command");
        send_ctl_resp(client_fd, &resp);
        close(client_fd);
        return;
    }

    generate_marker(marker, sizeof(marker));
    snprintf(wrapped, sizeof(wrapped), "%s\necho %s\n", req->data, marker);

    set_nonblocking(client_fd);
    s->exec_fd = client_fd;

    resp.status = 0;
    resp.id = s->id;
    resp.pid = s->pid;
    snprintf(resp.name, sizeof(resp.name), "%s", s->name);
    snprintf(resp.marker, sizeof(resp.marker), "%s", marker);
    if (send_ctl_resp(client_fd, &resp) < 0) {
        s->exec_fd = -1;
        close(client_fd);
        return;
    }

    write_all_fd(s->pty_fd, wrapped, strlen(wrapped));
}

static void daemon_handle_client(int client_fd) {
    struct ctl_req req;

    if (recv_ctl_req(client_fd, &req) < 0) {
        close(client_fd);
        return;
    }

    switch (req.cmd) {
    case CMD_NEW:
        handle_new(client_fd, &req);
        break;
    case CMD_LIST:
        handle_list(client_fd);
        break;
    case CMD_KILL:
        handle_kill(client_fd, &req);
        break;
    case CMD_ATTACH:
        handle_attach(client_fd, &req);
        break;
    case CMD_EXEC:
        handle_exec(client_fd, &req);
        break;
    default:
        close(client_fd);
        break;
    }
}

static void daemon_event_loop(int listen_fd) {
    for (;;) {
        struct pollfd pfds[1 + MAX_SESSIONS * 3];
        int pty_slot[1 + MAX_SESSIONS * 3];
        int client_slot[1 + MAX_SESSIONS * 3];
        int exec_slot[1 + MAX_SESSIONS * 3];
        int nfds = 0;
        int i;

        reap_children();

        pfds[nfds].fd = listen_fd;
        pfds[nfds].events = POLLIN;
        pty_slot[nfds] = -1;
        client_slot[nfds] = -1;
        exec_slot[nfds] = -1;
        nfds++;

        for (i = 0; i < MAX_SESSIONS; i++) {
            if (!sessions[i].used) {
                continue;
            }
            pfds[nfds].fd = sessions[i].pty_fd;
            pfds[nfds].events = POLLIN | POLLHUP | POLLERR;
            pty_slot[nfds] = i;
            client_slot[nfds] = -1;
            exec_slot[nfds] = -1;
            nfds++;
            if (sessions[i].client_fd >= 0) {
                pfds[nfds].fd = sessions[i].client_fd;
                pfds[nfds].events = POLLIN | POLLHUP | POLLERR;
                pty_slot[nfds] = -1;
                client_slot[nfds] = i;
                exec_slot[nfds] = -1;
                nfds++;
            }
            if (sessions[i].exec_fd >= 0) {
                pfds[nfds].fd = sessions[i].exec_fd;
                pfds[nfds].events = POLLHUP | POLLERR;
                pty_slot[nfds] = -1;
                client_slot[nfds] = -1;
                exec_slot[nfds] = i;
                nfds++;
            }
        }

        {
            int has_sessions = 0;
            int rc;
            for (i = 0; i < MAX_SESSIONS; i++) {
                if (sessions[i].used) { has_sessions = 1; break; }
            }
            rc = poll(pfds, nfds, has_sessions ? -1 : 10000);
            if (rc < 0) {
                if (errno == EINTR) {
                    continue;
                }
                break;
            }
            if (rc == 0 && !has_sessions) {
                break;
            }
        }

        for (i = 0; i < nfds; i++) {
            struct session *s;

            if (pfds[i].fd == listen_fd && (pfds[i].revents & POLLIN)) {
                int client_fd = accept(listen_fd, NULL, NULL);
                if (client_fd >= 0) {
                    daemon_handle_client(client_fd);
                }
                continue;
            }

            if (pty_slot[i] >= 0) {
                char buf[PAYLOAD_MAX];
                ssize_t n;

                s = find_session_by_slot(pty_slot[i]);
                if (!s) {
                    continue;
                }
                if (pfds[i].revents & (POLLHUP | POLLERR)) {
                    if (s->client_fd >= 0) {
                        send_packet(s->client_fd, PKT_EXIT, NULL, 0);
                    }
                    if (s->exec_fd >= 0) {
                        send_packet(s->exec_fd, PKT_EXIT, NULL, 0);
                    }
                    session_cleanup(s);
                    continue;
                }
                if (!(pfds[i].revents & POLLIN)) {
                    continue;
                }
                n = read(s->pty_fd, buf, sizeof(buf));
                if (n <= 0) {
                    if (n < 0 && (errno == EAGAIN || errno == EINTR)) {
                        continue;
                    }
                    if (s->client_fd >= 0) {
                        send_packet(s->client_fd, PKT_EXIT, NULL, 0);
                    }
                    if (s->exec_fd >= 0) {
                        send_packet(s->exec_fd, PKT_EXIT, NULL, 0);
                    }
                    session_cleanup(s);
                    continue;
                }
                session_append_backlog(s, buf, (size_t)n);
                if (s->client_fd >= 0 && send_packet(s->client_fd, PKT_DATA, buf, (uint32_t)n) < 0) {
                    session_detach_client(s);
                }
                if (s->exec_fd >= 0 && send_packet(s->exec_fd, PKT_DATA, buf, (uint32_t)n) < 0) {
                    close(s->exec_fd);
                    s->exec_fd = -1;
                }
                continue;
            }

            if (client_slot[i] >= 0) {
                struct packet pkt;
                int rc;

                s = find_session_by_slot(client_slot[i]);
                if (!s) {
                    continue;
                }
                if (pfds[i].revents & (POLLHUP | POLLERR)) {
                    session_detach_client(s);
                    continue;
                }
                if (!(pfds[i].revents & POLLIN)) {
                    continue;
                }
                rc = recv_packet(s->client_fd, &pkt);
                if (rc <= 0) {
                    session_detach_client(s);
                    continue;
                }
                if (pkt.type == PKT_DATA) {
                    write_all_fd(s->pty_fd, pkt.data, pkt.len);
                } else if (pkt.type == PKT_RESIZE && pkt.len == sizeof(struct winsize)) {
                    struct winsize ws;
                    memcpy(&ws, pkt.data, sizeof(ws));
                    ioctl(s->pty_fd, TIOCSWINSZ, &ws);
                }
                continue;
            }

            if (exec_slot[i] >= 0) {
                s = find_session_by_slot(exec_slot[i]);
                if (!s) {
                    continue;
                }
                if (pfds[i].revents & (POLLHUP | POLLERR)) {
                    close(s->exec_fd);
                    s->exec_fd = -1;
                }
            }
        }
    }
}

static void on_sigchld(int sig) { (void)sig; }

static void daemon_main(void) {
    int listen_fd;
    struct sigaction sa;

    signal(SIGPIPE, SIG_IGN);
    memset(&sa, 0, sizeof(sa));
    sa.sa_handler = on_sigchld;
    sa.sa_flags = SA_NOCLDSTOP;
    sigaction(SIGCHLD, &sa, NULL);

    listen_fd = daemon_socket_listen();
    if (listen_fd < 0) {
        _exit(1);
    }
    daemon_event_loop(listen_fd);
    close(listen_fd);
    {
        char path[108];
        format_socket_path(path, sizeof(path));
        unlink(path);
    }
    _exit(0);
}

static void restore_terminal(void) {
    if (termios_saved) {
        tcsetattr(STDIN_FILENO, TCSAFLUSH, &saved_termios);
        termios_saved = 0;
    }
}

static void on_resize(int sig) {
    (void)sig;
    resize_pending = 1;
}

static void on_fatal_signal(int sig) {
    restore_terminal();
    signal(sig, SIG_DFL);
    raise(sig);
}

static void setup_raw_mode(void) {
    struct termios raw;
    int sigs[] = {SIGTERM, SIGHUP, SIGINT, SIGQUIT, SIGABRT, SIGSEGV};
    size_t i;

    if (!isatty(STDIN_FILENO)) {
        die("stdin is not a tty");
    }
    if (tcgetattr(STDIN_FILENO, &saved_termios) < 0) {
        die("tcgetattr failed: %s", strerror(errno));
    }
    termios_saved = 1;
    atexit(restore_terminal);

    for (i = 0; i < sizeof(sigs) / sizeof(sigs[0]); i++) {
        struct sigaction sa;
        memset(&sa, 0, sizeof(sa));
        sa.sa_handler = on_fatal_signal;
        sigaction(sigs[i], &sa, NULL);
    }

    raw = saved_termios;
    cfmakeraw(&raw);
    if (tcsetattr(STDIN_FILENO, TCSAFLUSH, &raw) < 0) {
        die("tcsetattr failed: %s", strerror(errno));
    }
}

static void get_current_winsize(struct winsize *ws) {
    memset(ws, 0, sizeof(*ws));
    if (ioctl(STDIN_FILENO, TIOCGWINSZ, ws) < 0) {
        ws->ws_row = 24;
        ws->ws_col = 80;
    }
}

static int cmd_new(const char *name) {
    int fd;
    struct ctl_req req;
    struct ctl_resp resp;

    ensure_daemon_running();
    fd = connect_daemon();
    if (fd < 0) {
        die("connect daemon failed: %s", strerror(errno));
    }

    memset(&req, 0, sizeof(req));
    req.cmd = CMD_NEW;
    get_current_winsize(&req.ws);
    if (name) {
        snprintf(req.name, sizeof(req.name), "%s", name);
    }

    if (send_ctl_req(fd, &req) < 0 || recv_ctl_resp(fd, &resp) < 0) {
        die("daemon request failed");
    }
    close(fd);

    if (resp.status < 0) {
        die("%s", resp.msg[0] ? resp.msg : "create session failed");
    }
    printf("%s\t%d\n", resp.name, resp.id);
    return 0;
}

static int cmd_list(void) {
    int fd;
    struct ctl_req req;
    struct ctl_resp resp;

    ensure_daemon_running();
    fd = connect_daemon();
    if (fd < 0) {
        die("connect daemon failed: %s", strerror(errno));
    }

    memset(&req, 0, sizeof(req));
    req.cmd = CMD_LIST;
    if (send_ctl_req(fd, &req) < 0) {
        die("list request failed");
    }

    printf("ID\tNAME\tPID\n");
    while (recv_ctl_resp(fd, &resp) == 0) {
        if (!resp.more) {
            break;
        }
        printf("%d\t%s\t%d\n", resp.id, resp.name, resp.pid);
    }
    close(fd);
    return 0;
}

static int cmd_kill(const char *target) {
    int fd;
    struct ctl_req req;
    struct ctl_resp resp;

    ensure_daemon_running();
    fd = connect_daemon();
    if (fd < 0) {
        die("connect daemon failed: %s", strerror(errno));
    }

    memset(&req, 0, sizeof(req));
    req.cmd = CMD_KILL;
    if (is_numeric(target)) {
        req.id = atoi(target);
    } else {
        snprintf(req.name, sizeof(req.name), "%s", target);
    }

    if (send_ctl_req(fd, &req) < 0 || recv_ctl_resp(fd, &resp) < 0) {
        die("kill request failed");
    }
    close(fd);

    if (resp.status < 0) {
        die("%s", resp.msg[0] ? resp.msg : "kill failed");
    }
    return 0;
}

static int cmd_exec(const char *target, int timeout_sec, const char *command) {
    int fd;
    struct ctl_req req;
    struct ctl_resp resp;
    char marker[MARKER_LEN];
    char scan_buf[PAYLOAD_MAX * 2];
    size_t scan_len = 0;
    time_t deadline;

    ensure_daemon_running();
    fd = connect_daemon();
    if (fd < 0) {
        die("connect daemon failed: %s", strerror(errno));
    }

    memset(&req, 0, sizeof(req));
    req.cmd = CMD_EXEC;
    if (is_numeric(target)) {
        req.id = atoi(target);
    } else {
        strncpy(req.name, target, sizeof(req.name) - 1);
    }
    strncpy(req.data, command, sizeof(req.data) - 1);

    if (send_ctl_req(fd, &req) < 0 || recv_ctl_resp(fd, &resp) < 0) {
        die("exec request failed");
    }
    if (resp.status < 0) {
        close(fd);
        die("%s", resp.msg[0] ? resp.msg : "exec failed");
    }

    strncpy(marker, resp.marker, sizeof(marker) - 1);
    marker[sizeof(marker) - 1] = '\0';
    deadline = time(NULL) + timeout_sec;

    for (;;) {
        struct pollfd pfd;
        int remaining = (int)(deadline - time(NULL));
        struct packet pkt;
        int rc;

        if (remaining <= 0) {
            fprintf(stderr, "\nsimpterm: exec timed out\n");
            close(fd);
            return 1;
        }

        pfd.fd = fd;
        pfd.events = POLLIN | POLLHUP | POLLERR;

        rc = poll(&pfd, 1, remaining * 1000);
        if (rc < 0) {
            if (errno == EINTR) {
                continue;
            }
            break;
        }
        if (rc == 0) {
            fprintf(stderr, "\nsimpterm: exec timed out\n");
            close(fd);
            return 1;
        }

        if (pfd.revents & (POLLHUP | POLLERR)) {
            break;
        }

        rc = recv_packet(fd, &pkt);
        if (rc <= 0 || pkt.type == PKT_EXIT) {
            break;
        }

        if (pkt.type == PKT_DATA && pkt.len > 0) {
            /* Check if marker appears in this chunk or spanning prev+this */
            size_t marker_len = strlen(marker);
            size_t keep = marker_len > 1 ? marker_len - 1 : 0;
            size_t total;
            char *found;

            /* Append to scan buffer */
            if (scan_len + pkt.len > sizeof(scan_buf)) {
                /* Flush old data that can't contain marker start */
                size_t flush = scan_len + pkt.len - sizeof(scan_buf);
                if (flush > scan_len) {
                    flush = scan_len;
                }
                write_all_fd(STDOUT_FILENO, scan_buf, flush);
                memmove(scan_buf, scan_buf + flush, scan_len - flush);
                scan_len -= flush;
            }
            memcpy(scan_buf + scan_len, pkt.data, pkt.len);
            scan_len += pkt.len;

            found = memmem(scan_buf, scan_len, marker, marker_len);
            if (found) {
                /* Output everything before the marker line */
                /* Walk back to find the start of the marker line */
                char *line_start = found;
                while (line_start > scan_buf && *(line_start - 1) != '\n') {
                    line_start--;
                }
                if (line_start > scan_buf) {
                    write_all_fd(STDOUT_FILENO, scan_buf, (size_t)(line_start - scan_buf));
                }
                close(fd);
                return 0;
            }

            /* Flush data that can't be part of marker */
            total = scan_len;
            if (total > keep) {
                size_t flush = total - keep;
                write_all_fd(STDOUT_FILENO, scan_buf, flush);
                memmove(scan_buf, scan_buf + flush, keep);
                scan_len = keep;
            }
        }
    }

    /* Flush remaining scan buffer */
    if (scan_len > 0) {
        write_all_fd(STDOUT_FILENO, scan_buf, scan_len);
    }
    close(fd);
    return 0;
}

static void send_resize_if_needed(int fd, int force) {
    struct winsize ws;

    if (!force && !resize_pending) {
        return;
    }
    resize_pending = 0;
    get_current_winsize(&ws);
    send_packet(fd, PKT_RESIZE, &ws, sizeof(ws));
}

static int cmd_attach(const char *target) {
    int fd;
    struct ctl_req req;
    struct ctl_resp resp;
    struct sigaction sa;

    ensure_daemon_running();
    fd = connect_daemon();
    if (fd < 0) {
        die("connect daemon failed: %s", strerror(errno));
    }

    memset(&req, 0, sizeof(req));
    req.cmd = CMD_ATTACH;
    get_current_winsize(&req.ws);
    if (is_numeric(target)) {
        req.id = atoi(target);
    } else {
        snprintf(req.name, sizeof(req.name), "%s", target);
    }

    if (send_ctl_req(fd, &req) < 0 || recv_ctl_resp(fd, &resp) < 0) {
        die("attach request failed");
    }
    if (resp.status < 0) {
        close(fd);
        die("%s", resp.msg[0] ? resp.msg : "attach failed");
    }

    setup_raw_mode();
    memset(&sa, 0, sizeof(sa));
    sa.sa_handler = on_resize;
    sigaction(SIGWINCH, &sa, NULL);
    send_resize_if_needed(fd, 1);

    for (;;) {
        struct pollfd pfds[2];
        int rc;

        pfds[0].fd = STDIN_FILENO;
        pfds[0].events = POLLIN;
        pfds[1].fd = fd;
        pfds[1].events = POLLIN | POLLHUP | POLLERR;

        send_resize_if_needed(fd, 0);
        rc = poll(pfds, 2, -1);
        if (rc < 0) {
            if (errno == EINTR) {
                continue;
            }
            break;
        }

        if (pfds[0].revents & POLLIN) {
            char buf[PAYLOAD_MAX];
            ssize_t n = read(STDIN_FILENO, buf, sizeof(buf));
            ssize_t i;
            ssize_t chunk_start = 0;

            if (n <= 0) {
                break;
            }
            for (i = 0; i < n; i++) {
                if ((unsigned char)buf[i] == 0x1c) {
                    if (i > chunk_start) {
                        send_packet(fd, PKT_DATA, buf + chunk_start, (uint32_t)(i - chunk_start));
                    }
                    close(fd);
                    return 0;
                }
            }
            send_packet(fd, PKT_DATA, buf, (uint32_t)n);
        }

        if (pfds[1].revents & (POLLHUP | POLLERR)) {
            break;
        }
        if (pfds[1].revents & POLLIN) {
            struct packet pkt;
            int prc = recv_packet(fd, &pkt);
            if (prc <= 0 || pkt.type == PKT_EXIT) {
                break;
            }
            if (pkt.type == PKT_DATA && pkt.len > 0) {
                write_all_fd(STDOUT_FILENO, pkt.data, pkt.len);
            }
        }
    }

    close(fd);
    return 0;
}

static void usage(FILE *out) {
    fprintf(out,
            "usage:\n"
            "  simpterm n [name]                  new session\n"
            "  simpterm a <name|id>               attach\n"
            "  simpterm e <name|id> <timeout> <cmd> exec command\n"
            "  simpterm l                         list sessions\n"
            "  simpterm k <name|id>               kill session\n");
}

int main(int argc, char **argv) {
    if (argc >= 2 && strcmp(argv[1], "__daemon") == 0) {
        daemon_main();
        return 0;
    }

    if (argc < 2 || strlen(argv[1]) != 1) {
        usage(stderr);
        return 1;
    }

    switch (argv[1][0]) {
    case 'n': {
        const char *name = (argc >= 3) ? argv[2] : NULL;
        if (name && is_numeric(name)) {
            die("session name cannot be purely numeric");
        }
        return cmd_new(name);
    }
    case 'a':
        if (argc != 3) {
            usage(stderr);
            return 1;
        }
        return cmd_attach(argv[2]);
    case 'e':
        if (argc < 5) {
            usage(stderr);
            return 1;
        }
        if (!is_numeric(argv[3])) {
            die("timeout must be a number (seconds)");
        }
        return cmd_exec(argv[2], atoi(argv[3]), argv[4]);
    case 'l':
        return cmd_list();
    case 'k':
        if (argc != 3) {
            usage(stderr);
            return 1;
        }
        return cmd_kill(argv[2]);
    default:
        usage(stderr);
        return 1;
    }
}
