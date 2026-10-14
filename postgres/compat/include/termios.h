#ifndef PGMEM_TERMIOS_H
#define PGMEM_TERMIOS_H
typedef unsigned int tcflag_t;
struct termios
{
	tcflag_t	c_iflag;
	tcflag_t	c_oflag;
	tcflag_t	c_cflag;
	tcflag_t	c_lflag;
};
#define ECHO 0x8
#define TCSAFLUSH 2
int			tcgetattr(int fd, struct termios *t);
int			tcsetattr(int fd, int action, const struct termios *t);
#endif
