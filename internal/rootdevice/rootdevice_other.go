//go:build !linux

package rootdevice

type Store struct{}

func Inspect(string) (Filesystem, error)                { return Filesystem{}, ErrUnsupported }
func ValidateBase(string, string) (Base, error)         { return Base{}, ErrUnsupported }
func OpenStore(string, string) (*Store, Base, error)    { return nil, Base{}, ErrUnsupported }
func (*Store) Close() error                             { return ErrUnsupported }
func (*Store) Clone(string) (CloneResult, error)        { return CloneResult{}, ErrUnsupported }
func Clone(string, string, string) (CloneResult, error) { return CloneResult{}, ErrUnsupported }
func Delete(string, string) error                       { return ErrUnsupported }
func Recover(string) (int, error)                       { return 0, ErrUnsupported }
func SetBaseImmutable(string, bool) error               { return ErrUnsupported }
func DedicatedMount(string) error                       { return ErrUnsupported }
func ExerciseDiskFull(string, string, string) error     { return ErrUnsupported }
