#import <CoreServices/CoreServices.h>
#import <UserNotifications/UserNotifications.h>

#import "Notifications.h"

static void scheduleNotification(UNUserNotificationCenter *center, NSString *body, dispatch_block_t completion) {
    if (body == nil) {
        completion();
        return;
    }

    UNMutableNotificationContent *content = [UNMutableNotificationContent new];
    content.title = [NSBundle.mainBundle objectForInfoDictionaryKey:@"CFBundleName"];
    content.body = body;

    // Let this short-lived app exit before Notification Center presents the alert.
    UNTimeIntervalNotificationTrigger *trigger =
        [UNTimeIntervalNotificationTrigger triggerWithTimeInterval:1 repeats:NO];
    UNNotificationRequest *request = [UNNotificationRequest
        requestWithIdentifier:NSUUID.UUID.UUIDString content:content trigger:trigger];
    [center addNotificationRequest:request withCompletionHandler:^(NSError *error) {
        if (error) {
            NSLog(@"Cannot schedule notification: %@", error.localizedDescription);
        }
        completion();
    }];
}

void deliverNotification(NSString *body, dispatch_block_t completion) {
    OSStatus registration = LSRegisterURL((__bridge CFURLRef)NSBundle.mainBundle.bundleURL, false);
    if (registration != noErr) {
        NSLog(@"Cannot register notification app with Launch Services: %d", (int)registration);
        completion();
        return;
    }

    UNUserNotificationCenter *center = UNUserNotificationCenter.currentNotificationCenter;
    [center getNotificationSettingsWithCompletionHandler:^(UNNotificationSettings *settings) {
        if (settings.authorizationStatus == UNAuthorizationStatusNotDetermined) {
            [center requestAuthorizationWithOptions:UNAuthorizationOptionAlert
                completionHandler:^(BOOL granted, NSError *error) {
                    if (granted && error == nil) {
                        scheduleNotification(center, body, completion);
                    } else {
                        if (error) {
                            NSLog(@"Cannot request notifications: %@", error.localizedDescription);
                        }
                        completion();
                    }
                }];
        } else if (settings.authorizationStatus == UNAuthorizationStatusDenied) {
            completion();
        } else {
            scheduleNotification(center, body, completion);
        }
    }];
}
