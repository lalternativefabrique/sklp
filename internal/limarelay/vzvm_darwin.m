#import <Virtualization/Virtualization.h>
#import <Foundation/Foundation.h>

static VZVirtualMachine *gVM = nil;
static dispatch_queue_t gQueue = nil;

@interface SklpVMDelegate : NSObject <VZVirtualMachineDelegate>
@property (nonatomic) BOOL stopped;
@end

@implementation SklpVMDelegate
- (void)guestDidStopVirtualMachine:(VZVirtualMachine *)vm { self.stopped = YES; }
- (void)virtualMachine:(VZVirtualMachine *)vm didStopWithError:(NSError *)error {
    self.stopped = YES;
}
@end

static SklpVMDelegate *gDelegate = nil;

int sklp_vz_start(const char *kernelPath, const char *diskPath, const char *cmdline,
                  const char *shareDir, const char *shareTag,
                  const char *cacheDir, const char *cacheTag,
                  int cpuCount, unsigned long long memBytes,
                  int consoleFd, char *errBuf, int errLen) {
    @autoreleasepool {
        NSError *err = nil;

        VZLinuxBootLoader *boot = [[VZLinuxBootLoader alloc]
            initWithKernelURL:[NSURL fileURLWithPath:[NSString stringWithUTF8String:kernelPath]]];
        boot.commandLine = [NSString stringWithUTF8String:cmdline];

        VZVirtualMachineConfiguration *cfg = [[VZVirtualMachineConfiguration alloc] init];
        cfg.bootLoader = boot;
        cfg.CPUCount = cpuCount;
        cfg.memorySize = memBytes;

        VZVirtioConsoleDeviceSerialPortConfiguration *serial =
            [[VZVirtioConsoleDeviceSerialPortConfiguration alloc] init];
        serial.attachment = [[VZFileHandleSerialPortAttachment alloc]
            initWithFileHandleForReading:nil
                    fileHandleForWriting:[[NSFileHandle alloc] initWithFileDescriptor:consoleFd]];
        cfg.serialPorts = @[serial];

        VZDiskImageStorageDeviceAttachment *disk =
            [[VZDiskImageStorageDeviceAttachment alloc]
                initWithURL:[NSURL fileURLWithPath:[NSString stringWithUTF8String:diskPath]]
                   readOnly:NO error:&err];
        if (disk == nil) {
            snprintf(errBuf, errLen, "disk: %s", [[err localizedDescription] UTF8String]);
            return 1;
        }
        cfg.storageDevices = @[[[VZVirtioBlockDeviceConfiguration alloc] initWithAttachment:disk]];

        NSMutableArray *shares = [NSMutableArray array];
        if (shareDir && shareTag) {
            VZVirtioFileSystemDeviceConfiguration *d =
                [[VZVirtioFileSystemDeviceConfiguration alloc]
                    initWithTag:[NSString stringWithUTF8String:shareTag]];
            d.share = [[VZSingleDirectoryShare alloc] initWithDirectory:
                [[VZSharedDirectory alloc]
                    initWithURL:[NSURL fileURLWithPath:[NSString stringWithUTF8String:shareDir]]
                       readOnly:NO]];
            [shares addObject:d];
        }
        if (cacheDir && cacheTag) {
            VZVirtioFileSystemDeviceConfiguration *d =
                [[VZVirtioFileSystemDeviceConfiguration alloc]
                    initWithTag:[NSString stringWithUTF8String:cacheTag]];
            d.share = [[VZSingleDirectoryShare alloc] initWithDirectory:
                [[VZSharedDirectory alloc]
                    initWithURL:[NSURL fileURLWithPath:[NSString stringWithUTF8String:cacheDir]]
                       readOnly:NO]];
            [shares addObject:d];
        }
        cfg.directorySharingDevices = shares;

        VZVirtioNetworkDeviceConfiguration *net = [[VZVirtioNetworkDeviceConfiguration alloc] init];
        net.attachment = [[VZNATNetworkDeviceAttachment alloc] init];
        cfg.networkDevices = @[net];

        cfg.socketDevices = @[[[VZVirtioSocketDeviceConfiguration alloc] init]];
        cfg.entropyDevices = @[[[VZVirtioEntropyDeviceConfiguration alloc] init]];

        if (![cfg validateWithError:&err]) {
            snprintf(errBuf, errLen, "invalid config: %s", [[err localizedDescription] UTF8String]);
            return 1;
        }

        gQueue = dispatch_queue_create("sklp.vz", DISPATCH_QUEUE_SERIAL);
        gDelegate = [[SklpVMDelegate alloc] init];
        dispatch_sync(gQueue, ^{
            gVM = [[VZVirtualMachine alloc] initWithConfiguration:cfg queue:gQueue];
            gVM.delegate = gDelegate;
        });

        __block NSString *startErr = nil;
        dispatch_semaphore_t sem = dispatch_semaphore_create(0);
        dispatch_async(gQueue, ^{
            [gVM startWithCompletionHandler:^(NSError *e) {
                if (e) startErr = [e localizedDescription];
                dispatch_semaphore_signal(sem);
            }];
        });
        dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
        if (startErr) {
            snprintf(errBuf, errLen, "start: %s", [startErr UTF8String]);
            return 2;
        }
        return 0;
    }
}

// Connects to a vsock port the guest agent listens on. Returns an owned fd.
int sklp_vz_connect(unsigned int port, char *errBuf, int errLen) {
    @autoreleasepool {
        if (gVM == nil) { snprintf(errBuf, errLen, "vm not started"); return -1; }
        __block int outFd = -1;
        __block NSString *cerr = nil;
        dispatch_semaphore_t sem = dispatch_semaphore_create(0);
        dispatch_async(gQueue, ^{
            VZVirtioSocketDevice *dev = nil;
            for (VZSocketDevice *d in gVM.socketDevices)
                if ([d isKindOfClass:[VZVirtioSocketDevice class]]) { dev = (VZVirtioSocketDevice *)d; break; }
            if (dev == nil) { cerr = @"no vsock device"; dispatch_semaphore_signal(sem); return; }
            [dev connectToPort:port completionHandler:^(VZVirtioSocketConnection *c, NSError *e) {
                if (e) cerr = [e localizedDescription]; else outFd = dup(c.fileDescriptor);
                dispatch_semaphore_signal(sem);
            }];
        });
        dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
        if (outFd < 0) {
            snprintf(errBuf, errLen, "vsock connect: %s", cerr ? [cerr UTF8String] : "unknown");
            return -1;
        }
        return outFd;
    }
}

// Pumps the run loop until the guest powers off or timeoutSec elapses.
int sklp_vz_wait(int timeoutSec) {
    @autoreleasepool {
        NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:(double)timeoutSec];
        while (!gDelegate.stopped && [deadline timeIntervalSinceNow] > 0)
            [[NSRunLoop currentRunLoop] runMode:NSDefaultRunLoopMode
                                     beforeDate:[NSDate dateWithTimeIntervalSinceNow:0.05]];
        return gDelegate.stopped ? 0 : 1;
    }
}

// Non-blocking counterpart to sklp_vz_wait: reports whether the guest has
// already stopped. A VM that never came up (a rejected configuration) leaves
// gVM nil, which counts as stopped — the daemon must not keep host forwards
// open for a guest that is not there.
int sklp_vz_stopped(void) {
    if (gVM == nil || gDelegate == nil) return 1;
    return gDelegate.stopped ? 1 : 0;
}

void sklp_vz_stop(void) {
    if (gVM == nil) return;
    dispatch_async(gQueue, ^{
        if (gVM.canRequestStop) { NSError *e = nil; [gVM requestStopWithError:&e]; }
    });
}
