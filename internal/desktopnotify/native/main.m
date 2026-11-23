#import <AppKit/AppKit.h>
#import <CoreServices/CoreServices.h>
#import <UserNotifications/UserNotifications.h>

@interface Notifier : NSObject <NSApplicationDelegate>
@property(nonatomic, copy) NSString *body;
@end

@implementation Notifier

- (void)applicationDidFinishLaunching:(NSNotification *)notification {
    OSStatus registration = LSRegisterURL((__bridge CFURLRef)NSBundle.mainBundle.bundleURL, false);
    if (registration != noErr) {
        NSLog(@"Cannot register notification app with Launch Services: %d", (int)registration);
        [self finish];
        return;
    }
    UNUserNotificationCenter *center = UNUserNotificationCenter.currentNotificationCenter;
    [center requestAuthorizationWithOptions:UNAuthorizationOptionAlert
        completionHandler:^(BOOL granted, NSError *error) {
            if (!granted || error) {
                NSLog(@"Notification authorization unavailable: %@", error.localizedDescription ?: @"denied");
                [self finish];
                return;
            }

            UNMutableNotificationContent *content = [UNMutableNotificationContent new];
            content.title = [NSBundle.mainBundle objectForInfoDictionaryKey:@"CFBundleName"];
            content.body = self.body;

            // Let this short-lived app exit before Notification Center presents the alert.
            UNTimeIntervalNotificationTrigger *trigger =
                [UNTimeIntervalNotificationTrigger triggerWithTimeInterval:1 repeats:NO];
            UNNotificationRequest *request = [UNNotificationRequest
                requestWithIdentifier:NSUUID.UUID.UUIDString content:content trigger:trigger];
            [center addNotificationRequest:request withCompletionHandler:^(NSError *deliveryError) {
                if (deliveryError) {
                    NSLog(@"Cannot schedule notification: %@", deliveryError.localizedDescription);
                }
                [self finish];
            }];
        }];
}

- (void)finish {
    dispatch_async(dispatch_get_main_queue(), ^{ [NSApp terminate:nil]; });
}

@end

int main(int argc, const char *argv[]) {
    @autoreleasepool {
        NSString *body;
        if (argc == 3 && strcmp(argv[1], "--notify") == 0) {
            body = [NSString stringWithUTF8String:argv[2]];
        } else if (argc == 2 && strcmp(argv[1], "--test") == 0) {
            body = @"Notifications are working.";
        } else {
            return 0;
        }
        if (body.length == 0) {
            return 1;
        }

        NSApplication *app = NSApplication.sharedApplication;
        __attribute__((objc_precise_lifetime)) Notifier *notifier = [Notifier new];
        notifier.body = body;
        app.delegate = notifier;
        dispatch_after(dispatch_time(DISPATCH_TIME_NOW, 120 * NSEC_PER_SEC),
            dispatch_get_main_queue(), ^{ [app terminate:nil]; });
        [app run];
    }
    return 0;
}
