#import <AppKit/AppKit.h>

#import "Notifications.h"

@interface AppDelegate : NSObject <NSApplicationDelegate>
@property(nonatomic, copy, nullable) NSString *notificationBody;
@end

@implementation AppDelegate

- (void)applicationDidFinishLaunching:(NSNotification *)notification {
    deliverNotification(self.notificationBody, ^{
        dispatch_async(dispatch_get_main_queue(), ^{ [NSApp terminate:nil]; });
    });
}

@end

int main(int argc, const char *argv[]) {
    @autoreleasepool {
        NSString *body = nil;
        if (argc == 3 && strcmp(argv[1], "--notify") == 0) {
            body = [NSString stringWithUTF8String:argv[2]];
            if (body.length == 0) {
                return 1;
            }
        } else if (argc == 2 && strcmp(argv[1], "--test") == 0) {
            body = @"Notifications are working.";
        } else if (argc != 1 && !(argc == 2 && strncmp(argv[1], "-psn_", 5) == 0)) {
            return 1;
        }

        NSApplication *app = NSApplication.sharedApplication;
        __attribute__((objc_precise_lifetime)) AppDelegate *delegate = [AppDelegate new];
        delegate.notificationBody = body;
        app.delegate = delegate;
        dispatch_after(dispatch_time(DISPATCH_TIME_NOW, 120 * NSEC_PER_SEC),
            dispatch_get_main_queue(), ^{ [app terminate:nil]; });
        [app run];
    }
    return 0;
}
