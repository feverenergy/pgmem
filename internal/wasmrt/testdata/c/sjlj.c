/* setjmp/longjmp across frames, lowered to WebAssembly exceptions. */
#include <setjmp.h>
#include <stdio.h>

static jmp_buf jb;

static int
depth(int n)
{
	if (n == 0)
		longjmp(jb, 42);
	return depth(n - 1) + 1;
}

int
main(void)
{
	int			r = setjmp(jb);

	if (r == 0)
	{
		depth(10);
		puts("longjmp did not unwind");
		return 1;
	}
	printf("longjmp %d\n", r);
	return 0;
}
